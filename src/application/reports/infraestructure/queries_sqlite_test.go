//go:build sidecar

package infraestructure_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	"github.com/cuadra/cuadra-core/src/application/reports"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// Fixture mínima para el reader: todas las migraciones reales + gym +
// operador (payments.operator_id y members.created_by son NOT NULL FK).
type readerFixture struct {
	db         *sqlx.DB
	uow        sharedDomain.UnitOfWork
	gymID      uuid.UUID
	operatorID uuid.UUID
	planID     uuid.UUID
}

func setupReaderDB(t *testing.T) *readerFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlx.Open("sqlite3", filepath.Join(dir, "test.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	migDir := "../../../../db_migrations/sqlite"
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(migDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("apply %s: %v", e.Name(), err)
		}
	}

	f := &readerFixture{
		db:         db,
		uow:        sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue()),
		gymID:      uuid.New(),
		operatorID: uuid.New(),
		planID:     uuid.New(),
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name)
		VALUES (?, ?, 1, ?, ?, 'Gym Test')`,
		f.gymID, f.gymID, now, now); err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role)
		VALUES (?, ?, 1, ?, ?, 'op@t.mx', 'x', 'Operador', 'operator')`,
		f.operatorID, f.gymID, now, now); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO membership_types
		  (id, gym_id, version, created_at, updated_at, name, price, duration_days, active)
		VALUES (?, ?, 1, ?, ?, 'Mensual', 50000, 30, 1)`,
		f.planID, f.gymID, now, now); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	return f
}

func TestSQLiteReader_FinancialMetadataUsesMutationWatermarkAndLocalQueue(t *testing.T) {
	f := setupReaderDB(t)
	f.seedPayment(t, nil, "other", 0, "2026-08-10", false)
	want := time.Date(2026, 8, 24, 17, 30, 0, 0, time.UTC)
	if _, err := f.db.Exec(`UPDATE payments SET updated_at=? WHERE gym_id=?`, want.UnixMilli(), f.gymID.String()); err != nil {
		t.Fatal(err)
	}
	queueID := uuid.New().String()
	if _, err := f.db.Exec(`INSERT INTO sync_queue(id,entity_type,entity_id,operation,payload,client_version,enqueued_at)
		VALUES(?, 'payments', ?, 'upsert', ?, 1, ?)`, queueID, uuid.New().String(),
		`{"gym_id":"`+f.gymID.String()+`"}`, want.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := reportsInfra.NewSQLiteReader().FinancialReadMetadata(tx, f.gymID)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.DataWatermark == nil || !metadata.DataWatermark.Equal(want) {
		t.Fatalf("data watermark=%v, want %v", metadata.DataWatermark, want)
	}
	if metadata.SyncPending == nil || !*metadata.SyncPending {
		t.Fatalf("sync pending=%v, want true", metadata.SyncPending)
	}
	if _, err := f.db.Exec(`UPDATE sync_queue SET synced_at=? WHERE id=?`, want.Add(time.Minute).UnixMilli(), queueID); err != nil {
		t.Fatal(err)
	}
	tx, err = f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = reportsInfra.NewSQLiteReader().FinancialReadMetadata(tx, f.gymID)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SyncPending == nil || *metadata.SyncPending {
		t.Fatalf("sync pending after ack=%v, want false", metadata.SyncPending)
	}
}

func TestSQLiteReader_LegacyPurchaseWithKnownCostIsCompleteAndCounted(t *testing.T) {
	f := setupReaderDB(t)
	at := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	productID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO products
		(id,gym_id,version,created_at,updated_at,name,price,stock)
		VALUES(?,?,1,?,?,'Agua legacy',1500,13)`,
		productID, f.gymID, at.UnixMilli(), at.UnixMilli()); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	knownID, unknownID, captureID := uuid.New(), uuid.New(), uuid.New()
	if _, err := f.db.Exec(`INSERT INTO stock_movements
		(id,gym_id,version,created_at,updated_at,product_id,movement_type,delta,cost,operator_id,is_purchase)
		VALUES
		(?,?,1,?,?,?,?,10,500,?,1),
		(?,?,1,?,?,?,?,3,NULL,?,1),
		(?,?,1,?,?,?,?,7,250,?,0)`,
		knownID, f.gymID, at.UnixMilli(), at.UnixMilli(), productID, "restock", f.operatorID,
		unknownID, f.gymID, at.Add(time.Minute).UnixMilli(), at.Add(time.Minute).UnixMilli(), productID, "restock", f.operatorID,
		captureID, f.gymID, at.Add(2*time.Minute).UnixMilli(), at.Add(2*time.Minute).UnixMilli(), productID, "restock", f.operatorID); err != nil {
		t.Fatalf("seed legacy stock movements: %v", err)
	}
	// Some migrated rows already have a placeholder aggregate whose zeros
	// mean "unknown". The surviving stock journal still makes this amount
	// exact, so the placeholder must not hide quantity × unit cost.
	if _, err := f.db.Exec(`INSERT INTO inventory_purchases
		(id,gym_id,version,created_at,updated_at,stock_movement_id,product_id,quantity,
		 unit_cost,total_amount,status,idempotency_key,created_by)
		VALUES(?,?,1,?,?,?,?,10,0,0,'legacy_incomplete',?,?)`,
		uuid.New(), f.gymID, at.UnixMilli(), at.UnixMilli(), knownID, productID,
		"legacy-known-cost", f.operatorID); err != nil {
		t.Fatalf("seed legacy purchase placeholder: %v", err)
	}

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := reportsInfra.NewSQLiteReader()
	day := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	total, err := reader.SumInventoryCostBetween(tx, f.gymID, "America/Mexico_City", day, day)
	if err != nil {
		t.Fatal(err)
	}
	if total != 50 {
		t.Fatalf("legacy purchase total=%v, want 50", total)
	}
	snapshot, err := reader.CanonicalFinancialBetween(tx, f.gymID, "America/Mexico_City", day, day)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.InventoryPurchases != 50 || snapshot.LegacyPurchaseCount != 1 {
		t.Fatalf("snapshot=%+v, want purchases=50 and one truly missing amount", snapshot)
	}
	series, err := reader.ExpensesDailySeries(tx, f.gymID, "America/Mexico_City", day, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Date.Format("2006-01-02") != "2026-08-22" || series[0].Total != 50 {
		t.Fatalf("expense series=%+v, want 2026-08-22/$50", series)
	}
	rows, err := reader.ListInventoryCostsBetween(tx, f.gymID, "America/Mexico_City", day, day, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MovementID != knownID || rows[0].CostUnit != 5 || rows[0].CostTotal != 50 {
		t.Fatalf("legacy purchase detail=%+v", rows)
	}
}

func TestSQLiteReader_OtherIncomeAndCashActivity(t *testing.T) {
	f := setupReaderDB(t)
	f.seedPayment(t, nil, "other", 0, "2026-08-10", false)
	if _, err := f.db.Exec(`UPDATE payments SET created_at=? WHERE gym_id=?`,
		time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC).UnixMilli(), f.gymID.String()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixMilli()
	for _, row := range []struct {
		day        string
		calculated int64
		counted    any
		deleted    any
	}{
		{"2026-08-10", 10_000, int64(12_500), nil},
		{"2026-08-11", 5_000, nil, nil},
		{"2026-08-12", 99_000, int64(99_000), now},
	} {
		openedAt, parseErr := time.Parse("2006-01-02", row.day)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		status := "closed_unverified"
		var reconciledAt, reconciledBy any
		if row.counted != nil {
			status = "reconciled"
			reconciledAt, reconciledBy = now, f.operatorID
		}
		if _, err := f.db.Exec(`INSERT INTO cash_close_events
			(id,gym_id,version,created_at,updated_at,deleted_at,close_date,calculated_cash,counted_cash,closed_by,
			drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,
			activity_cash,adjusted_after_withdrawal,opened_at,opened_by,closed_at,reconciled_at,reconciled_by)
			VALUES(?,?,1,?,?,?,?,?,?,?,?,?,?,1,?,0,1,?,0,?,?,?,?,?)`,
			uuid.New(), f.gymID, now, now, row.deleted, row.day, row.calculated, row.counted, f.operatorID,
			f.gymID, "main", row.day, status, row.calculated, openedAt.UnixMilli(), f.operatorID, now, reconciledAt, reconciledBy); err != nil {
			t.Fatalf("seed cash close: %v", err)
		}
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := reportsInfra.NewSQLiteReader()
	from := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	other, err := reader.SumOtherIncomeBetween(tx, f.gymID, from, to)
	if err != nil || other != 100 {
		t.Fatalf("other income = %.2f, err=%v; want 100", other, err)
	}
	activity, err := reader.SumCashClosedBetween(tx, f.gymID, from, to)
	if err != nil || activity != 100 {
		t.Fatalf("cash activity = %.2f, err=%v; want 100 from all physical cash operations, without counts or discrepancies", activity, err)
	}
	counted, err := reader.SumCashCountedBetween(tx, f.gymID, from, to)
	if err != nil {
		t.Fatalf("cash counted: %v", err)
	}
	if counted.Counted != 125 || counted.CountedCloses != 1 || counted.TotalCloses != 2 ||
		counted.ActiveDays != 1 || counted.MissingActiveDays != 0 || counted.UncoveredActivityDays != 1 ||
		counted.TotalSessions != 2 || counted.OpenSessions != 0 ||
		counted.ReconciledSessions != 1 || counted.ClosedUnverifiedSessions != 0 ||
		counted.StaleSessions != 1 || counted.WithdrawnSessions != 0 {
		t.Fatalf("cash counted = %+v; the $125 count stays valid while an orphan $50 snapshot is exposed as stale", counted)
	}
}

func TestSQLiteReader_CashGoldenKeepsPeriodFlowSeparateFromLatestPhysicalCount(t *testing.T) {
	f := setupReaderDB(t)
	day := "2026-08-24"
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC).UnixMilli()
	paymentID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO payments
		(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,cash_drawer_id,
		 concept,balance_pending,payment_date,operator_id)
		VALUES(?,?,1,?,?,?,50000,50000,'cash',?,'membership',0,?,?)`,
		paymentID, f.gymID, now, now, "P-"+paymentID.String()[:8], f.gymID, day, f.operatorID); err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO cash_close_events
		(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
		 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
		 cash_left,withdrawn_cash,withdrawal_destination,adjusted_after_withdrawal,opened_at,opened_by,closed_at,
		 reconciled_at,reconciled_by,withdrawn_at,withdrawn_by)
		VALUES(?,?,1,?,?,?,70000,70000,?,?,'main',?,1,'withdrawn',20000,1,50000,
		 20000,50000,'gym_fund',0,?,?,?,?,?,?,?)`,
		sessionID, f.gymID, now, now, day, f.operatorID, f.gymID, day,
		now-1000, f.operatorID, now, now, f.operatorID, now, f.operatorID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := reportsInfra.NewSQLiteReader()
	from := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	activity, err := reader.SumCashClosedBetween(tx, f.gymID, from, from)
	if err != nil {
		t.Fatal(err)
	}
	r, err := reader.SumCashCountedBetween(tx, f.gymID, from, from)
	if err != nil {
		t.Fatal(err)
	}
	if activity != 500 || r.Withdrawn != 500 || r.LatestSessionID == nil || *r.LatestSessionID != sessionID ||
		r.LatestExpected == nil || *r.LatestExpected != 700 || r.LatestCounted == nil || *r.LatestCounted != 700 ||
		r.LatestDifference == nil || *r.LatestDifference != 0 {
		t.Fatalf("activity=%.2f reconciliation=%+v; want 500/500 and latest 700/700/0", activity, r)
	}
	if _, err = f.db.Exec(`UPDATE cash_close_events SET adjusted_after_withdrawal=1,
		integrity_note='corrección histórica',version=version+1,updated_at=? WHERE id=?`, now+1, sessionID); err != nil {
		t.Fatal(err)
	}
	tx, err = f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adjusted, err := reader.SumCashCountedBetween(tx, f.gymID, from, from)
	if err != nil {
		t.Fatal(err)
	}
	if adjusted.Withdrawn != 500 || adjusted.WithdrawnSessions != 1 || adjusted.StaleSessions != 0 ||
		adjusted.AdjustedAfterWithdrawalSessions != 1 || adjusted.LatestStatus != "withdrawn" ||
		!adjusted.LatestNeedsRecount || adjusted.LatestDifference != nil {
		t.Fatalf("adjusted withdrawal=%+v; transfer remains historical but its difference is no longer certified", adjusted)
	}
}

func TestSQLiteReader_TwoDrawersDoNotDuplicatePeriodActivityAndLatestIsOneSession(t *testing.T) {
	f := setupReaderDB(t)
	day := "2026-08-25"
	now := time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC).UnixMilli()
	customDrawerID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO cash_drawers
		(id,gym_id,version,created_at,updated_at,code,name,active,is_main)
		VALUES(?,?,1,?,?,'secondary','Caja secundaria',1,0)`, customDrawerID, f.gymID, now, now); err != nil {
		t.Fatal(err)
	}
	insertPayment := func(amount int64, drawerID uuid.UUID) {
		t.Helper()
		id := uuid.New()
		if _, err := f.db.Exec(`INSERT INTO payments
			(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,cash_drawer_id,
			 concept,balance_pending,payment_date,operator_id)
			VALUES(?,?,1,?,?,?, ?,?,'cash',?,'membership',0,?,?)`,
			id, f.gymID, now, now, "P-"+id.String()[:8], amount, amount, drawerID, day, f.operatorID); err != nil {
			t.Fatal(err)
		}
	}
	insertPayment(50_000, f.gymID)
	insertPayment(10_000, customDrawerID)
	insertSession := func(id, drawerID uuid.UUID, code string, sequence int, at, opening, activity, counted, left int64) {
		t.Helper()
		withdrawn := counted - left
		if _, err := f.db.Exec(`INSERT INTO cash_close_events
			(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
			 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
			 cash_left,withdrawn_cash,withdrawal_destination,adjusted_after_withdrawal,opened_at,opened_by,closed_at,
			 reconciled_at,reconciled_by,withdrawn_at,withdrawn_by)
			VALUES(?,?,1,?,?,?, ?,?,?, ?,?,?,?,'withdrawn',?,1,?, ?,?,'gym_fund',0,?,?,?,?,?,?,?)`,
			id, f.gymID, at, at, day, opening+activity, counted, f.operatorID,
			drawerID, code, day, sequence, opening, activity, left, withdrawn,
			at-1000, f.operatorID, at, at, f.operatorID, at, f.operatorID); err != nil {
			t.Fatal(err)
		}
	}
	mainSessionID, customSessionID := uuid.New(), uuid.New()
	// Sequence is local to one drawer and therefore cannot rank two drawers.
	// The secondary drawer was counted later even though main is on sequence 2.
	insertSession(mainSessionID, f.gymID, "main", 2, now, 20_000, 50_000, 70_000, 20_000)
	insertSession(customSessionID, customDrawerID, "secondary", 1, now+1000, 5_000, 10_000, 15_000, 5_000)

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dayTime := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	reader := reportsInfra.NewSQLiteReader()
	activity, err := reader.SumCashClosedBetween(tx, f.gymID, dayTime, dayTime)
	if err != nil {
		t.Fatal(err)
	}
	r, err := reader.SumCashCountedBetween(tx, f.gymID, dayTime, dayTime)
	if err != nil {
		t.Fatal(err)
	}
	if activity != 600 || r.Withdrawn != 600 || r.TotalCloses != 2 ||
		r.LatestSessionID == nil || *r.LatestSessionID != customSessionID ||
		r.LatestExpected == nil || *r.LatestExpected != 150 ||
		r.LatestCounted == nil || *r.LatestCounted != 150 ||
		r.LatestDifference == nil || *r.LatestDifference != 0 {
		t.Fatalf("activity=%.2f reconciliation=%+v; two drawers must total 600 once and latest custom 150/150/0", activity, r)
	}
}

func TestSQLiteReader_OpenSessionActivityIsIncludedAndCoverageRemainsIncomplete(t *testing.T) {
	f := setupReaderDB(t)
	day := "2026-08-26"
	base := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC).UnixMilli()
	insertPayment := func(amount, createdAt int64) {
		t.Helper()
		id := uuid.New()
		if _, err := f.db.Exec(`INSERT INTO payments
			(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,cash_drawer_id,
			 concept,balance_pending,payment_date,operator_id)
			VALUES(?,?,1,?,?,?, ?,?,'cash',?,'membership',0,?,?)`,
			id, f.gymID, createdAt, createdAt, "P-"+id.String()[:8], amount, amount, f.gymID, day, f.operatorID); err != nil {
			t.Fatal(err)
		}
	}
	insertPayment(50_000, base)
	insertPayment(5_000, base+300)
	closedID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO cash_close_events
		(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
		 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
		 cash_left,withdrawn_cash,withdrawal_destination,adjusted_after_withdrawal,opened_at,opened_by,closed_at,
		 reconciled_at,reconciled_by,withdrawn_at,withdrawn_by)
		VALUES(?,?,1,?,?,?,50000,50000,?,?,'main',?,1,'withdrawn',0,1,50000,
		 0,50000,'gym_fund',0,?,?,?,?,?,?,?)`,
		closedID, f.gymID, base, base+100, day, f.operatorID, f.gymID, day,
		base-1000, f.operatorID, base+100, base+100, f.operatorID, base+100, f.operatorID); err != nil {
		t.Fatal(err)
	}
	openID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO cash_close_events
		(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,closed_by,
		 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
		 adjusted_after_withdrawal,opened_at,opened_by)
		VALUES(?,?,1,?,?,?,0,?,?,'main',?,2,'open',0,1,0,0,?,?)`,
		openID, f.gymID, base+200, base+200, day, f.operatorID, f.gymID, day, base+200, f.operatorID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := reportsInfra.NewSQLiteReader()
	date := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	activity, err := reader.SumCashClosedBetween(tx, f.gymID, date, date)
	if err != nil {
		t.Fatal(err)
	}
	reconciliation, err := reader.SumCashCountedBetween(tx, f.gymID, date, date)
	if err != nil {
		t.Fatal(err)
	}
	if activity != 550 || reconciliation.TotalCloses != 1 || reconciliation.CountedCloses != 1 ||
		reconciliation.TotalSessions != 2 || reconciliation.OpenSessions != 1 ||
		reconciliation.WithdrawnSessions != 1 || reconciliation.ActiveDays != 1 ||
		reconciliation.LatestSessionID == nil || *reconciliation.LatestSessionID != closedID ||
		reconciliation.LatestStatus != "withdrawn" {
		t.Fatalf("activity=%.2f reconciliation=%+v; open activity must be visible while the session stays incomplete", activity, reconciliation)
	}
}

func TestSQLiteReader_LateCashBeforeWithdrawalMakesLatestCloseExplicitlyStale(t *testing.T) {
	f := setupReaderDB(t)
	day := "2026-08-27"
	base := time.Date(2026, 8, 27, 20, 0, 0, 0, time.UTC).UnixMilli()
	for i, row := range []struct {
		amount, createdAt int64
	}{{50_000, base}, {5_000, base + 300}} {
		id := uuid.New()
		if _, err := f.db.Exec(`INSERT INTO payments
			(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,cash_drawer_id,
			 concept,balance_pending,payment_date,operator_id)
			VALUES(?,?,1,?,?,?, ?,?,'cash',?,'membership',0,?,?)`,
			id, f.gymID, row.createdAt, row.createdAt, "P-"+strconv.Itoa(i), row.amount, row.amount, f.gymID, day, f.operatorID); err != nil {
			t.Fatal(err)
		}
	}
	sessionID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO cash_close_events
		(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
		 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
		 adjusted_after_withdrawal,opened_at,opened_by,closed_at,reconciled_at,reconciled_by)
		VALUES(?,?,2,?,?,?,50000,50000,?,?,'main',?,1,'reconciled',0,1,50000,
		 0,?,?,?,?,?)`,
		sessionID, f.gymID, base-1000, base+300, day, f.operatorID, f.gymID, day,
		base-1000, f.operatorID, base+100, base+100, f.operatorID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := reportsInfra.NewSQLiteReader()
	date := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	activity, err := reader.SumCashClosedBetween(tx, f.gymID, date, date)
	if err != nil {
		t.Fatal(err)
	}
	reconciliation, err := reader.SumCashCountedBetween(tx, f.gymID, date, date)
	if err != nil {
		t.Fatal(err)
	}
	if activity != 550 || reconciliation.TotalCloses != 1 || reconciliation.CountedCloses != 0 ||
		reconciliation.TotalSessions != 1 || reconciliation.StaleSessions != 1 ||
		reconciliation.ReconciledSessions != 0 || reconciliation.ActiveDays != 1 ||
		reconciliation.UncoveredActivityDays != 1 ||
		reconciliation.LatestStatus != "stale" || !reconciliation.LatestNeedsRecount ||
		reconciliation.LatestCounted == nil || *reconciliation.LatestCounted != 500 ||
		reconciliation.LatestDifference != nil {
		t.Fatalf("activity=%.2f reconciliation=%+v; stale close must include late flow but suppress obsolete difference", activity, reconciliation)
	}
}

func TestSQLiteReader_UpdatedCashRecordAfterCloseInvalidatesRangeSnapshot(t *testing.T) {
	f := setupReaderDB(t)
	day := "2026-08-28"
	base := time.Date(2026, 8, 28, 20, 0, 0, 0, time.UTC).UnixMilli()
	paymentID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO payments
		(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,cash_drawer_id,
		 concept,balance_pending,payment_date,operator_id)
		VALUES(?,?,1,?,?,?,50000,50000,'cash',?,'membership',0,?,?)`,
		paymentID, f.gymID, base, base, "P-corrected", f.gymID, day, f.operatorID); err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO cash_close_events
		(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
		 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
		 adjusted_after_withdrawal,opened_at,opened_by,closed_at,reconciled_at,reconciled_by)
		VALUES(?,?,1,?,?,?,50000,50000,?,?,'main',?,1,'reconciled',0,1,50000,
		 0,?,?,?,?,?)`,
		sessionID, f.gymID, base-1000, base+100, day, f.operatorID, f.gymID, day,
		base-1000, f.operatorID, base+100, base+100, f.operatorID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE payments SET amount=40000,recognized_amount=40000,
		version=version+1,updated_at=? WHERE id=?`, base+300, paymentID); err != nil {
		t.Fatal(err)
	}

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	reader := reportsInfra.NewSQLiteReader()
	activity, err := reader.SumCashClosedBetween(tx, f.gymID, date, date)
	if err != nil {
		t.Fatal(err)
	}
	reconciliation, err := reader.SumCashCountedBetween(tx, f.gymID, date, date)
	if err != nil {
		t.Fatal(err)
	}
	if activity != 400 || reconciliation.TotalSessions != 1 ||
		reconciliation.StaleSessions != 1 || reconciliation.ReconciledSessions != 0 ||
		reconciliation.UncoveredActivityDays != 1 || reconciliation.LatestStatus != "stale" ||
		!reconciliation.LatestNeedsRecount || reconciliation.LatestCounted == nil ||
		*reconciliation.LatestCounted != 500 || reconciliation.LatestDifference != nil {
		t.Fatalf("activity=%.2f reconciliation=%+v; an updated cash row must invalidate the old range count", activity, reconciliation)
	}
}

func TestSQLiteReader_UpdatedCashRecordAfterWithdrawalKeepsTransferAndVoidsDifference(t *testing.T) {
	f := setupReaderDB(t)
	day := "2026-09-01"
	base := time.Date(2026, 9, 1, 20, 0, 0, 0, time.UTC).UnixMilli()
	paymentID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO payments
		(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,cash_drawer_id,
		 concept,balance_pending,payment_date,operator_id)
		VALUES(?,?,1,?,?,?,50000,50000,'cash',?,'membership',0,?,?)`,
		paymentID, f.gymID, base, base, "P-withdrawn-corrected", f.gymID, day, f.operatorID); err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO cash_close_events
		(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
		 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
		 cash_left,withdrawn_cash,withdrawal_destination,adjusted_after_withdrawal,opened_at,opened_by,
		 closed_at,reconciled_at,reconciled_by,withdrawn_at,withdrawn_by)
		VALUES(?,?,1,?,?,?,50000,50000,?,?,'main',?,1,'withdrawn',0,1,50000,
		 0,50000,'gym_fund',0,?,?,?,?,?,?,?)`,
		sessionID, f.gymID, base-1000, base+200, day, f.operatorID, f.gymID, day,
		base-1000, f.operatorID, base+100, base+100, f.operatorID, base+200, f.operatorID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE payments SET amount=40000,recognized_amount=40000,
		version=version+1,updated_at=? WHERE id=?`, base+300, paymentID); err != nil {
		t.Fatal(err)
	}

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	reader := reportsInfra.NewSQLiteReader()
	reconciliation, err := reader.SumCashCountedBetween(tx, f.gymID, date, date)
	if err != nil {
		t.Fatal(err)
	}
	if reconciliation.Withdrawn != 500 || reconciliation.WithdrawnSessions != 1 ||
		reconciliation.StaleSessions != 0 || reconciliation.UncoveredActivityDays != 1 ||
		reconciliation.AdjustedAfterWithdrawalSessions != 1 ||
		reconciliation.LatestStatus != "withdrawn" || !reconciliation.LatestNeedsRecount ||
		reconciliation.LatestDifference != nil {
		t.Fatalf("reconciliation=%+v; the historical transfer stays immutable while its difference becomes uncertified", reconciliation)
	}
}

func TestSQLiteReader_CashCoverageDistinguishesNoActivityFromHistoryWithoutSession(t *testing.T) {
	f := setupReaderDB(t)
	f.seedPayment(t, nil, "other", 0, "2026-08-29", false)
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := reportsInfra.NewSQLiteReader()
	emptyDate := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	empty, err := reader.SumCashCountedBetween(tx, f.gymID, emptyDate, emptyDate)
	if err != nil {
		t.Fatal(err)
	}
	if empty.ActiveDays != 0 || empty.TotalSessions != 0 || empty.MissingActiveDays != 0 || empty.UncoveredActivityDays != 0 {
		t.Fatalf("no-activity coverage=%+v, want all zero", empty)
	}
	activeDate := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	activity, err := reader.SumCashClosedBetween(tx, f.gymID, activeDate, activeDate)
	if err != nil {
		t.Fatal(err)
	}
	uncovered, err := reader.SumCashCountedBetween(tx, f.gymID, activeDate, activeDate)
	if err != nil {
		t.Fatal(err)
	}
	if activity != 100 || uncovered.ActiveDays != 1 || uncovered.TotalSessions != 0 ||
		uncovered.MissingActiveDays != 0 || uncovered.UncoveredActivityDays != 0 || uncovered.HistoricalActivityDays != 1 {
		t.Fatalf("activity=%.2f cash-without-session coverage=%+v, want visible 100 and one historical day without a retroactive pending cut", activity, uncovered)
	}
}

func TestSQLiteReader_CashCoverageFailsClosedWhenEventTimestampPrecedesSessionWindow(t *testing.T) {
	f := setupReaderDB(t)
	day := "2026-08-30"
	openedAt := time.Date(2026, 8, 30, 6, 0, 0, 0, time.UTC).UnixMilli()
	paymentID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO payments
		(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,cash_drawer_id,
		 concept,balance_pending,payment_date,operator_id)
		VALUES(?,?,1,?,?,?,10000,10000,'cash',?,'membership',0,?,?)`,
		paymentID, f.gymID, openedAt-1, openedAt-1, "P-"+paymentID.String()[:8], f.gymID, day, f.operatorID); err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO cash_close_events
		(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
		 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
		 adjusted_after_withdrawal,opened_at,opened_by,closed_at,reconciled_at,reconciled_by)
		VALUES(?,?,1,?,?,?,0,0,?,?,'main',?,1,'reconciled',0,1,0,0,?,?,?,?,?)`,
		sessionID, f.gymID, openedAt, openedAt+1000, day, f.operatorID, f.gymID, day,
		openedAt, f.operatorID, openedAt+1000, openedAt+1000, f.operatorID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	reader := reportsInfra.NewSQLiteReader()
	activity, err := reader.SumCashClosedBetween(tx, f.gymID, date, date)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := reader.SumCashCountedBetween(tx, f.gymID, date, date)
	if err != nil {
		t.Fatal(err)
	}
	if activity != 100 || coverage.ActiveDays != 1 || coverage.TotalSessions != 1 ||
		coverage.UncoveredActivityDays != 1 || coverage.ReconciledSessions != 1 {
		t.Fatalf("activity=%.2f coverage=%+v; a timestamp outside every session window must remain visible and uncovered", activity, coverage)
	}
}

func TestSQLiteReader_PostWithdrawalActivityIsVisibleButUncoveredUntilNextSession(t *testing.T) {
	f := setupReaderDB(t)
	day := "2026-08-31"
	base := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC).UnixMilli()
	for index, event := range []struct{ amount, at int64 }{{50_000, base}, {5_000, base + 300}} {
		id := uuid.New()
		if _, err := f.db.Exec(`INSERT INTO payments
			(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,cash_drawer_id,
			 concept,balance_pending,payment_date,operator_id)
			VALUES(?,?,1,?,?,?, ?,?,'cash',?,'membership',0,?,?)`,
			id, f.gymID, event.at, event.at, "P-post-"+strconv.Itoa(index), event.amount, event.amount, f.gymID, day, f.operatorID); err != nil {
			t.Fatal(err)
		}
	}
	sessionID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO cash_close_events
		(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
		 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
		 cash_left,withdrawn_cash,withdrawal_destination,adjusted_after_withdrawal,opened_at,opened_by,closed_at,
		 reconciled_at,reconciled_by,withdrawn_at,withdrawn_by)
		VALUES(?,?,1,?,?,?,50000,50000,?,?,'main',?,1,'withdrawn',0,1,50000,
		 0,50000,'gym_fund',0,?,?,?,?,?,?,?)`,
		sessionID, f.gymID, base, base+200, day, f.operatorID, f.gymID, day,
		base-1000, f.operatorID, base+100, base+100, f.operatorID, base+200, f.operatorID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	reader := reportsInfra.NewSQLiteReader()
	activity, err := reader.SumCashClosedBetween(tx, f.gymID, date, date)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := reader.SumCashCountedBetween(tx, f.gymID, date, date)
	if err != nil {
		t.Fatal(err)
	}
	if activity != 550 || coverage.UncoveredActivityDays != 1 || coverage.TotalSessions != 1 ||
		coverage.WithdrawnSessions != 1 || coverage.LatestStatus != "withdrawn" {
		t.Fatalf("activity=%.2f coverage=%+v; post-withdrawal cash needs a new session and cannot disappear", activity, coverage)
	}
}

func TestSQLiteReader_IncomeComponentsIncludeMembershipSettlements(t *testing.T) {
	f := setupReaderDB(t)
	memberID := f.seedMember(t, "Ana")
	f.seedMembership(t, memberID, "2026-07-01", "2026-07-31", "active")
	now := time.Now().UTC().UnixMilli()
	parentID := uuid.New()
	if _, err := f.db.Exec(`
		INSERT INTO payments
		  (id,gym_id,version,created_at,updated_at,folio,member_id,amount,recognized_amount,payment_method,concept,balance_pending,payment_date,operator_id)
		VALUES(?,?,1,?,?,?, ?,40000,40000,'cash','membership',10000,'2026-07-01',?)`,
		parentID, f.gymID, now, now, "P-"+parentID.String()[:8], memberID, f.operatorID); err != nil {
		t.Fatalf("seed membership payment: %v", err)
	}
	settlementID := uuid.New()
	if _, err := f.db.Exec(`
		INSERT INTO payments
		  (id,gym_id,version,created_at,updated_at,folio,member_id,amount,recognized_amount,payment_method,concept,balance_pending,parent_payment_id,payment_date,operator_id)
		VALUES(?,?,1,?,?,?, ?,10000,10000,'cash','balance_settlement',0,?,'2026-07-10',?)`,
		settlementID, f.gymID, now, now, "P-"+settlementID.String()[:8], memberID, parentID, f.operatorID); err != nil {
		t.Fatalf("seed membership settlement: %v", err)
	}
	f.seedProductSale(t, 1500, "2026-07-12", []int{1}, false)
	f.seedPayment(t, nil, "other", 0, "2026-07-15", false)

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := reportsInfra.NewSQLiteReader()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	total, err := reader.SumPaymentsBetween(tx, f.gymID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	memberships, err := reader.IncomeByMembershipTypeBetween(tx, f.gymID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	products, err := reader.SumProductSalesBetween(tx, f.gymID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	other, err := reader.SumOtherIncomeBetween(tx, f.gymID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if memberships["Mensual"] != 500 {
		t.Fatalf("membership income = %.2f, want 500 including settlement", memberships["Mensual"])
	}
	components := memberships["Mensual"] + products.Amount + other
	if total != components {
		t.Fatalf("income %.2f != memberships %.2f + products %.2f + other %.2f", total, memberships["Mensual"], products.Amount, other)
	}
}

func TestSQLiteReader_CanonicalFinancialEquationIsCentSafe(t *testing.T) {
	f := setupReaderDB(t)
	now := time.Now().UTC().UnixMilli()
	day := "2026-08-24"
	for i, amount := range []int64{10, 20} {
		id := uuid.New()
		if _, err := f.db.Exec(`INSERT INTO payments
			(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,
			 concept,balance_pending,payment_date,operator_id)
			VALUES(?,?,1,?,?,?, ?,?,'transfer','other',0,?,?)`,
			id, f.gymID, now+int64(i), now+int64(i), "P-"+id.String()[:8], amount, amount, day, f.operatorID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(`INSERT INTO expenses
		(id,gym_id,version,created_at,updated_at,expense_date,amount,category,payment_method,created_by)
		VALUES(?,?,1,?,?,?,10,'otros','transfer',?)`, uuid.New(), f.gymID, now, now, day, f.operatorID); err != nil {
		t.Fatal(err)
	}
	productID, movementID := uuid.New(), uuid.New()
	if _, err := f.db.Exec(`INSERT INTO products
		(id,gym_id,version,created_at,updated_at,name,price,stock)
		VALUES(?,?,1,?,?,'Centavos',100,1)`, productID, f.gymID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO stock_movements
		(id,gym_id,version,created_at,updated_at,product_id,movement_type,delta,cost,operator_id,is_purchase)
		VALUES(?,?,1,?,?,?,?,1,20,?,1)`, movementID, f.gymID, now, now, productID, "restock", f.operatorID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO inventory_purchases
		(id,gym_id,version,created_at,updated_at,stock_movement_id,product_id,quantity,unit_cost,total_amount,
		 status,paid_on,payment_method,paid_from,idempotency_key,created_by)
		VALUES(?,?,1,?,?,?,?,1,20,20,'paid',?,'transfer','gym_fund',?,?)`,
		uuid.New(), f.gymID, now, now, movementID, productID, day, "cent-safe-purchase", f.operatorID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	snapshot, err := reportsInfra.NewSQLiteReader().CanonicalFinancialBetween(tx, f.gymID, "America/Mexico_City", date, date)
	if err != nil {
		t.Fatal(err)
	}
	incomeCents := int64(math.Round((snapshot.MembershipIncome + snapshot.ProductIncome + snapshot.OtherIncome + snapshot.UnclassifiedIncome) * 100))
	outflowCents := int64(math.Round((snapshot.OperatingExpenses + snapshot.InventoryPurchases + snapshot.Refunds) * 100))
	if incomeCents != 30 || outflowCents != 30 || incomeCents-outflowCents != 0 {
		t.Fatalf("canonical=%+v cents income/outflows/result=%d/%d/%d, want 30/30/0",
			snapshot, incomeCents, outflowCents, incomeCents-outflowCents)
	}
}

func TestSQLiteReader_LegacyCashExpenseIsVisibleButNeverAssumedPhysical(t *testing.T) {
	f := setupReaderDB(t)
	now := time.Date(2026, 8, 24, 18, 0, 0, 0, time.UTC).UnixMilli()
	day := "2026-08-24"
	expenseID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO expenses
		(id,gym_id,version,created_at,updated_at,expense_date,amount,category,payment_method,paid_from,created_by)
		VALUES(?,?,1,?,?,?,5000,'otros','cash','cash_register',?)`,
		expenseID, f.gymID, now, now, day, f.operatorID); err != nil {
		t.Fatal(err)
	}

	read := func() (reports.CanonicalFinancialSnapshot, reports.CashCountSummary, float64) {
		t.Helper()
		tx, err := f.uow.Query(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		reader := reportsInfra.NewSQLiteReader()
		date := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
		canonical, err := reader.CanonicalFinancialBetween(tx, f.gymID, "America/Mexico_City", date, date)
		if err != nil {
			t.Fatal(err)
		}
		coverage, err := reader.SumCashCountedBetween(tx, f.gymID, date, date)
		if err != nil {
			t.Fatal(err)
		}
		activity, err := reader.SumCashClosedBetween(tx, f.gymID, date, date)
		if err != nil {
			t.Fatal(err)
		}
		return canonical, coverage, activity
	}

	canonical, coverage, activity := read()
	if canonical.OperatingExpenses != 50 || canonical.LegacyCashSourceUnverifiedCount != 1 ||
		coverage.LegacyCashSourceUnverifiedCount != 1 || activity != 0 {
		t.Fatalf("before repair canonical=%+v coverage=%+v activity=%.2f; expense must count in result but not be invented as drawer activity", canonical, coverage, activity)
	}

	movementID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO cash_movements
		(id,gym_id,version,created_at,updated_at,movement_on,amount,movement_type,reason,operator_id,expense_id,classification_status,cash_drawer_id)
		VALUES(?,?,1,?,?,?,?, 'cash_out','Origen histórico confirmado',?,?,'expense',?)`,
		movementID, f.gymID, now+1, now+1, day, 5000, f.operatorID, expenseID, f.gymID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE expenses SET cash_movement_id=?,updated_at=? WHERE id=?`, movementID, now+1, expenseID); err != nil {
		t.Fatal(err)
	}

	canonical, coverage, activity = read()
	if canonical.LegacyCashSourceUnverifiedCount != 0 || coverage.LegacyCashSourceUnverifiedCount != 0 || activity != -50 {
		t.Fatalf("after Caja repair canonical=%+v coverage=%+v activity=%.2f, want warning cleared and one physical outflow", canonical, coverage, activity)
	}
}

func TestSQLiteReader_LegacyRepairToDrawerFailsClosedAgainstHistoricalSessions(t *testing.T) {
	f := setupReaderDB(t)
	reader := reportsInfra.NewSQLiteReader()

	seedRepair := func(day string, base int64, status string) {
		t.Helper()
		expenseID := uuid.New()
		if _, err := f.db.Exec(`INSERT INTO expenses
			(id,gym_id,version,created_at,updated_at,expense_date,amount,category,payment_method,paid_from,created_by)
			VALUES(?,?,1,?,?,?,5000,'otros','cash','cash_register',?)`,
			expenseID, f.gymID, base-2000, base-2000, day, f.operatorID); err != nil {
			t.Fatal(err)
		}
		sessionID := uuid.New()
		if status == "withdrawn" {
			if _, err := f.db.Exec(`INSERT INTO cash_close_events
				(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
				 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
				 cash_left,withdrawn_cash,withdrawal_destination,adjusted_after_withdrawal,opened_at,opened_by,
				 closed_at,reconciled_at,reconciled_by,withdrawn_at,withdrawn_by)
				VALUES(?,?,1,?,?,?,0,0,?,?,'main',?,1,'withdrawn',0,1,0,
				 0,0,'gym_fund',0,?,?,?,?,?,?,?)`,
				sessionID, f.gymID, base-1000, base+200, day, f.operatorID, f.gymID, day,
				base-2000, f.operatorID, base, base, f.operatorID, base+100, f.operatorID); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := f.db.Exec(`INSERT INTO cash_close_events
				(id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
				 drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,activity_cash,
				 adjusted_after_withdrawal,opened_at,opened_by,closed_at,reconciled_at,reconciled_by)
				VALUES(?,?,1,?,?,?,0,0,?,?,'main',?,1,'reconciled',0,1,0,0,?,?,?,?,?)`,
				sessionID, f.gymID, base-1000, base, day, f.operatorID, f.gymID, day,
				base-2000, f.operatorID, base, base, f.operatorID); err != nil {
				t.Fatal(err)
			}
		}

		// Exact persistence shape produced when the owner confirms a migrated
		// unlinked expense as Caja: the economic day is historical, while the
		// audit/creation timestamp is now, after the old certification boundary.
		movementID := uuid.New()
		if _, err := f.db.Exec(`INSERT INTO cash_movements
			(id,gym_id,version,created_at,updated_at,movement_on,amount,movement_type,reason,operator_id,expense_id,classification_status,cash_drawer_id)
			VALUES(?,?,1,?,?,?,?, 'cash_out','Origen histórico confirmado',?,?,'expense',?)`,
			movementID, f.gymID, base+1000, base+1000, day, 5000, f.operatorID, expenseID, f.gymID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`UPDATE expenses SET cash_movement_id=?,updated_at=? WHERE id=?`, movementID, base+1000, expenseID); err != nil {
			t.Fatal(err)
		}
	}

	closedDay := "2026-08-20"
	closedBase := time.Date(2026, 8, 20, 20, 0, 0, 0, time.UTC).UnixMilli()
	seedRepair(closedDay, closedBase, "reconciled")
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closedDate := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	closedCoverage, err := reader.SumCashCountedBetween(tx, f.gymID, closedDate, closedDate)
	if err != nil {
		t.Fatal(err)
	}
	if closedCoverage.StaleSessions != 1 || closedCoverage.UncoveredActivityDays != 1 ||
		closedCoverage.LatestStatus != "stale" || closedCoverage.LegacyCashSourceUnverifiedCount != 0 {
		t.Fatalf("closed repair coverage=%+v; a new historical movement must invalidate the old certification", closedCoverage)
	}

	withdrawnDay := "2026-08-21"
	withdrawnBase := time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC).UnixMilli()
	seedRepair(withdrawnDay, withdrawnBase, "withdrawn")
	tx, err = f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	withdrawnDate := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	withdrawnCoverage, err := reader.SumCashCountedBetween(tx, f.gymID, withdrawnDate, withdrawnDate)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawnCoverage.WithdrawnSessions != 1 || withdrawnCoverage.UncoveredActivityDays != 1 ||
		withdrawnCoverage.StaleSessions != 0 || withdrawnCoverage.LegacyCashSourceUnverifiedCount != 0 {
		t.Fatalf("withdrawn repair coverage=%+v; post-withdrawal history stays uncovered and can never re-certify the retired session", withdrawnCoverage)
	}
}

func TestSQLiteReader_CanonicalFinancialEquationMatchesThe415Scenario(t *testing.T) {
	f := setupReaderDB(t)
	now := time.Now().UTC().UnixMilli()
	reportDay := "2026-08-23"
	memberID := f.seedMember(t, "Caso 415")

	for _, payment := range []struct {
		amount  int64
		concept string
		member  any
	}{
		{amount: 40_000, concept: "membership", member: memberID},
		{amount: 1_500, concept: "product", member: nil},
	} {
		id := uuid.New()
		if _, err := f.db.Exec(`INSERT INTO payments
			(id,gym_id,version,created_at,updated_at,folio,member_id,amount,recognized_amount,payment_method,
			 concept,balance_pending,payment_date,operator_id)
			VALUES(?,?,1,?,?,?,?,?,?,'transfer',?,0,?,?)`,
			id, f.gymID, now, now, "P-"+id.String()[:8], payment.member, payment.amount,
			payment.amount, payment.concept, reportDay, f.operatorID); err != nil {
			t.Fatalf("seed %s payment: %v", payment.concept, err)
		}
	}
	if _, err := f.db.Exec(`INSERT INTO expenses
		(id,gym_id,version,created_at,updated_at,expense_date,amount,category,payment_method,paid_from,created_by)
		VALUES(?,?,1,?,?,?,150000,'renta','transfer','gym_fund',?)`,
		uuid.New(), f.gymID, now, now, reportDay, f.operatorID); err != nil {
		t.Fatalf("seed expense: %v", err)
	}

	productID, movementID := uuid.New(), uuid.New()
	if _, err := f.db.Exec(`INSERT INTO products
		(id,gym_id,version,created_at,updated_at,name,price,stock)
		VALUES(?,?,1,?,?,'Compra golden',1500,10)`, productID, f.gymID, now, now); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := f.db.Exec(`INSERT INTO stock_movements
		(id,gym_id,version,created_at,updated_at,product_id,movement_type,delta,cost,operator_id,is_purchase)
		VALUES(?,?,1,?,?,?,?,10,500,?,1)`,
		movementID, f.gymID, now, now, productID, "restock", f.operatorID); err != nil {
		t.Fatalf("seed purchase stock movement: %v", err)
	}
	if _, err := f.db.Exec(`INSERT INTO inventory_purchases
		(id,gym_id,version,created_at,updated_at,stock_movement_id,product_id,quantity,unit_cost,total_amount,
		 status,paid_on,payment_method,paid_from,idempotency_key,created_by)
		VALUES(?,?,1,?,?,?,?,10,500,5000,'paid',?,'transfer','gym_fund',?,?)`,
		uuid.New(), f.gymID, now, now, movementID, productID, reportDay,
		"golden-415-purchase", f.operatorID); err != nil {
		t.Fatalf("seed paid inventory purchase: %v", err)
	}

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	snapshot, err := reportsInfra.NewSQLiteReader().CanonicalFinancialBetween(
		tx, f.gymID, "America/Mexico_City", date, date,
	)
	if err != nil {
		t.Fatal(err)
	}
	incomeCents := int64(math.Round((snapshot.MembershipIncome + snapshot.ProductIncome + snapshot.OtherIncome + snapshot.UnclassifiedIncome) * 100))
	outflowCents := int64(math.Round((snapshot.OperatingExpenses + snapshot.InventoryPurchases + snapshot.Refunds) * 100))
	resultCents := incomeCents - outflowCents
	if incomeCents != 41_500 || outflowCents != 155_000 || resultCents != -113_500 {
		t.Fatalf("canonical=%+v cents income/outflows/result=%d/%d/%d, want 41500/155000/-113500",
			snapshot, incomeCents, outflowCents, resultCents)
	}
}

func TestSQLiteReader_ProductProfitabilityIsCentExactAcrossDiscountInstallmentAndReturnDisposition(t *testing.T) {
	f := setupReaderDB(t)
	now := time.Now().UTC().UnixMilli()
	rootID, settlementID, saleID := uuid.New(), uuid.New(), uuid.New()
	if _, err := f.db.Exec(`INSERT INTO payments
		(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,
		 concept,balance_pending,payment_date,operator_id,discount_amount)
		VALUES(?,?,1,?,?,?,2,2,'cash','product',0,'2026-08-20',?,1)`,
		rootID, f.gymID, now, now, "CENT-ROOT", f.operatorID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO payments
		(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,
		 concept,balance_pending,parent_payment_id,payment_date,operator_id)
		VALUES(?,?,1,?,?,?,3,3,'transfer','balance_settlement',0,?,'2026-08-21',?)`,
		settlementID, f.gymID, now+1, now+1, "CENT-SETTLE", rootID, f.operatorID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO sales
		(id,gym_id,version,created_at,updated_at,payment_id,subtotal,discount,total)
		VALUES(?,?,1,?,?,?,?,1,5)`, saleID, f.gymID, now, now, rootID, 6); err != nil {
		t.Fatal(err)
	}

	lineIDs := make([]uuid.UUID, 3)
	for i := range lineIDs {
		productID := uuid.New()
		lineIDs[i] = uuid.New()
		if _, err := f.db.Exec(`INSERT INTO products
			(id,gym_id,version,created_at,updated_at,name,price,stock)
			VALUES(?,?,1,?,?,?,2,1)`, productID, f.gymID, now, now, "Cent "+strconv.Itoa(i+1)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`INSERT INTO sale_items
			(id,gym_id,version,created_at,updated_at,sale_id,product_id,product_name_snapshot,
			 unit_price_snapshot,unit_cost_snapshot,quantity,line_total)
			VALUES(?,?,1,?,?,?,?,?,2,1,1,2)`, lineIDs[i], f.gymID, now, now, saleID, productID, "Cent "+strconv.Itoa(i+1)); err != nil {
			t.Fatal(err)
		}
	}

	refundID := uuid.New()
	if _, err := f.db.Exec(`INSERT INTO refunds
		(id,gym_id,version,created_at,updated_at,root_payment_id,sale_id,amount,method,refunded_on,
		 reason,kind,balance_cancelled,legacy_incomplete,idempotency_key,idempotency_fingerprint,created_by)
		VALUES(?,?,1,?,?,?,?,3,'cash','2026-08-22','Dos piezas','revenue_refund',0,0,?,?,?)`,
		refundID, f.gymID, now+2, now+2, rootID, saleID, "cent-refund", "cent-refund-fingerprint", f.operatorID); err != nil {
		t.Fatal(err)
	}
	for i, item := range []struct {
		amount      int64
		disposition string
	}{{2, "returned_to_stock"}, {1, "damaged"}} {
		if _, err := f.db.Exec(`INSERT INTO refund_items
			(id,gym_id,version,created_at,updated_at,refund_id,sale_item_id,quantity,amount,disposition)
			VALUES(?,?,1,?,?,?,?,1,?,?)`, uuid.New(), f.gymID, now+2, now+2, refundID, lineIDs[i], item.amount, item.disposition); err != nil {
			t.Fatal(err)
		}
	}

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := reportsInfra.NewSQLiteReader()
	read := func(from, to string) (revenue, cogs float64, quantity int) {
		t.Helper()
		fromDay, _ := time.Parse("2006-01-02", from)
		toDay, _ := time.Parse("2006-01-02", to)
		rows, readErr := reader.ProductProfitabilityBetween(tx, f.gymID, fromDay, toDay)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, row := range rows {
			revenue += row.Revenue
			cogs += row.COGS
			quantity += row.Quantity
			if !row.CostComplete || (row.Revenue != 0 && row.MarginPct == nil) {
				t.Fatalf("complete cost row expected: %+v", row)
			}
		}
		return math.Round(revenue*100) / 100, math.Round(cogs*100) / 100, quantity
	}

	if revenue, cogs, quantity := read("2026-08-20", "2026-08-21"); revenue != .05 || cogs != .03 || quantity != 3 {
		t.Fatalf("sale+settlement = revenue %.2f, cogs %.2f, qty %d; want .05/.03/3", revenue, cogs, quantity)
	}
	if revenue, cogs, quantity := read("2026-08-21", "2026-08-21"); revenue != .03 || cogs != .02 || quantity != 0 {
		t.Fatalf("settlement day = revenue %.2f, cogs %.2f, qty %d; want .03/.02/0", revenue, cogs, quantity)
	}
	if revenue, cogs, quantity := read("2026-08-22", "2026-08-22"); revenue != -.03 || cogs != -.01 || quantity != -2 {
		t.Fatalf("refund day = revenue %.2f, cogs %.2f, qty %d; want -.03/-.01/-2", revenue, cogs, quantity)
	}
	if revenue, cogs, quantity := read("2026-08-20", "2026-08-22"); revenue != .02 || cogs != .02 || quantity != 1 {
		t.Fatalf("net period = revenue %.2f, cogs %.2f, qty %d; want .02/.02/1", revenue, cogs, quantity)
	}
	from, _ := time.Parse("2006-01-02", "2026-08-20")
	to, _ := time.Parse("2006-01-02", "2026-08-22")
	canonical, err := reader.CanonicalFinancialBetween(tx, f.gymID, "America/Mexico_City", from, to)
	if err != nil {
		t.Fatal(err)
	}
	productCents := int64(math.Round(canonical.ProductIncome * 100))
	refundCents := int64(math.Round(canonical.Refunds * 100))
	if productCents != 5 || refundCents != 3 || productCents-refundCents != 2 {
		t.Fatalf("canonical product/refund/net = %.2f/%.2f/%.2f, want .05/.03/.02",
			canonical.ProductIncome, canonical.Refunds, canonical.ProductIncome-canonical.Refunds)
	}
}

func TestSQLiteReader_IncomeByMembershipTypeUsesExactEarlyRenewal(t *testing.T) {
	f := setupReaderDB(t)
	memberID := f.seedMember(t, "Ana")
	f.seedMembership(t, memberID, "2026-07-01", "2026-07-31", "active")
	if _, err := f.db.Exec(`UPDATE memberships SET status='replaced' WHERE member_id=?`, memberID); err != nil {
		t.Fatalf("mark current membership replaced: %v", err)
	}

	now := time.Now().UTC().UnixMilli()
	annualMembershipID := uuid.New()
	if _, err := f.db.Exec(`
		INSERT INTO memberships
		  (id, gym_id, version, created_at, updated_at, member_id, membership_type_id,
		   type_name_snapshot, price_snapshot, duration_days_snapshot, start_date, expiry_date, status)
		VALUES (?, ?, 1, ?, ?, ?, ?, 'Anual', 50000, 365, '2026-08-01', '2027-07-31', 'active')`,
		annualMembershipID, f.gymID, now, now, memberID, f.planID); err != nil {
		t.Fatalf("seed early-renewal membership: %v", err)
	}

	parentID := uuid.New()
	if _, err := f.db.Exec(`
		INSERT INTO payments
		  (id,gym_id,version,created_at,updated_at,folio,member_id,membership_id,
		   amount,recognized_amount,payment_method,concept,balance_pending,payment_date,operator_id)
		VALUES(?,?,1,?,?,?, ?,?,40000,40000,'cash','membership',10000,'2026-07-20',?)`,
		parentID, f.gymID, now, now, "P-"+parentID.String()[:8], memberID, annualMembershipID, f.operatorID); err != nil {
		t.Fatalf("seed early-renewal payment: %v", err)
	}
	settlementID := uuid.New()
	if _, err := f.db.Exec(`
		INSERT INTO payments
		  (id,gym_id,version,created_at,updated_at,folio,member_id,membership_id,
		   amount,recognized_amount,payment_method,concept,balance_pending,parent_payment_id,payment_date,operator_id)
		VALUES(?,?,1,?,?,?, ?,?,10000,10000,'cash','balance_settlement',0,?,'2026-07-25',?)`,
		settlementID, f.gymID, now, now, "P-"+settlementID.String()[:8], memberID, annualMembershipID, parentID, f.operatorID); err != nil {
		t.Fatalf("seed early-renewal settlement: %v", err)
	}

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := reportsInfra.NewSQLiteReader().IncomeByMembershipTypeBetween(tx, f.gymID,
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got["Anual"] != 500 || got["Mensual"] != 0 {
		t.Fatalf("income by type = %#v, want Anual=500; payment date precedes future membership start", got)
	}
}

func (f *readerFixture) seedMembership(t *testing.T, memberID uuid.UUID, start, expiry, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC().UnixMilli()
	if _, err := f.db.Exec(`
		INSERT INTO memberships
		  (id, gym_id, version, created_at, updated_at, member_id, membership_type_id,
		   type_name_snapshot, price_snapshot, duration_days_snapshot, start_date, expiry_date, status)
		VALUES (?, ?, 1, ?, ?, ?, ?, 'Mensual', 50000, 30, ?, ?, ?)`,
		id, f.gymID, now, now, memberID, f.planID, start, expiry, status); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	return id
}

func (f *readerFixture) seedMember(t *testing.T, fullName string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC().UnixMilli()
	if _, err := f.db.Exec(`
		INSERT INTO members (id, gym_id, version, created_at, updated_at, folio, full_name, phone, created_by)
		VALUES (?, ?, 1, ?, ?, ?, ?, '5512345678', ?)`,
		id, f.gymID, now, now, "SOC-"+id.String()[:8], fullName, f.operatorID); err != nil {
		t.Fatalf("seed member %s: %v", fullName, err)
	}
	return id
}

// seedPayment inserta un pago con la misma forma que el resto de la suite de
// integración. memberID nil = venta walk-in sin socio. balanceCents en
// centavos (así lo guarda SQLite); deleted marca soft-delete.
func (f *readerFixture) seedPayment(t *testing.T, memberID *uuid.UUID, concept string, balanceCents int64, paymentDate string, deleted bool) {
	t.Helper()
	var deletedAt any
	if deleted {
		deletedAt = time.Now().UTC().UnixMilli()
	}
	var mid any
	if memberID != nil {
		mid = memberID.String()
	}
	id := uuid.New()
	if _, err := f.db.Exec(`
		INSERT INTO payments
		   (id, gym_id, version, created_at, updated_at, deleted_at, folio, member_id,
		    amount, recognized_amount, payment_method, concept, balance_pending, payment_date, operator_id)
		 VALUES (?, ?, 1, 1, 1, ?, ?, ?, 10000, 10000, 'cash', ?, ?, ?, ?)`,
		id, f.gymID, deletedAt, "P-"+id.String()[:8], mid, concept, balanceCents, paymentDate, f.operatorID); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
}

func (f *readerFixture) listPendingBalances(t *testing.T) []reports.PendingBalanceRow {
	t.Helper()
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := reportsInfra.NewSQLiteReader().ListPendingBalances(tx, f.gymID)
	if err != nil {
		t.Fatalf("ListPendingBalances: %v", err)
	}
	return rows
}

// Regresión del bug de "Saldos pendientes": la versión rn=1 tomaba sólo el
// pago MÁS RECIENTE del socio. Un socio con deuda vieja que luego hacía una
// compra pagada completa desaparecía de la lista de deudores (su pago más
// reciente traía balance 0). La deuda vieja DEBE seguir apareciendo.
func TestListPendingBalances_DeudaViejaSobreviveAPagoNuevoSaldado(t *testing.T) {
	f := setupReaderDB(t)
	ana := f.seedMember(t, "Ana")
	f.seedPayment(t, &ana, "membership", 15000, "2026-05-01", false) // debe $150
	f.seedPayment(t, &ana, "product", 0, "2026-07-01", false)        // compra nueva, pagada completa

	rows := f.listPendingBalances(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (la deuda vieja no puede desaparecer)", len(rows))
	}
	got := rows[0]
	if got.MemberID != ana {
		t.Errorf("member = %s, want Ana (%s)", got.MemberID, ana)
	}
	if got.BalancePending != 150.00 {
		t.Errorf("balance = %v, want 150.00", got.BalancePending)
	}
	if want := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC); !got.PaymentDate.Equal(want) {
		t.Errorf("payment_date = %v, want %v (debe desde la deuda abierta más vieja)", got.PaymentDate, want)
	}
}

func TestListPendingBalances_SumaTodasLasDeudasDelSocio(t *testing.T) {
	f := setupReaderDB(t)

	// Bruno: dos deudas vivas → una sola fila con la SUMA y la fecha de la
	// más vieja. Además una deuda soft-deleted que NO debe contar.
	bruno := f.seedMember(t, "Bruno")
	f.seedPayment(t, &bruno, "membership", 10000, "2026-06-15", false)
	f.seedPayment(t, &bruno, "product", 5025, "2026-06-20", false)
	f.seedPayment(t, &bruno, "product", 99900, "2026-06-01", true) // soft-deleted

	// Ana: deuda única + otro cobro posterior saldado. El segundo cobro no
	// puede ocultar la obligación anterior.
	ana := f.seedMember(t, "Ana")
	f.seedPayment(t, &ana, "membership", 15000, "2026-05-01", false)
	f.seedPayment(t, &ana, "membership", 0, "2026-06-30", false)

	// Carla: sin deuda → ausente.
	carla := f.seedMember(t, "Carla")
	f.seedPayment(t, &carla, "membership", 0, "2026-06-10", false)

	// Venta walk-in fiada sin socio: no puede aparecer (ni reventar el JOIN).
	f.seedPayment(t, nil, "product", 7777, "2026-06-25", false)

	rows := f.listPendingBalances(t)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (Bruno y Ana)", len(rows))
	}

	// Orden: mayor deuda primero (Bruno $150.25 > Ana $150.00).
	if rows[0].MemberID != bruno || rows[1].MemberID != ana {
		t.Fatalf("orden = [%s, %s], want [Bruno, Ana] (balance DESC)", rows[0].FullName, rows[1].FullName)
	}
	if rows[0].BalancePending != 150.25 {
		t.Errorf("Bruno balance = %v, want 150.25 (10000+5025 centavos, sin la soft-deleted)", rows[0].BalancePending)
	}
	if want := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC); !rows[0].PaymentDate.Equal(want) {
		t.Errorf("Bruno payment_date = %v, want %v", rows[0].PaymentDate, want)
	}
	if rows[1].BalancePending != 150.00 {
		t.Errorf("Ana balance = %v, want 150.00", rows[1].BalancePending)
	}
}

// seedProductSale inserta la tríada payment(concept='product') + sale +
// sale_items con la que register_sale materializa una venta. amountCents es
// el total del pago; quantities es una entrada por línea de venta. deleted
// soft-borra las tres piezas (una venta cancelada de origen).
func (f *readerFixture) seedProductSale(t *testing.T, amountCents int64, paymentDate string, quantities []int, deleted bool) {
	t.Helper()
	now := time.Now().UTC().UnixMilli()
	var deletedAt any
	if deleted {
		deletedAt = now
	}
	payID := uuid.New()
	if _, err := f.db.Exec(`
		INSERT INTO payments
		   (id, gym_id, version, created_at, updated_at, deleted_at, folio, member_id,
		    amount, recognized_amount, payment_method, concept, balance_pending, payment_date, operator_id)
		 VALUES (?, ?, 1, ?, ?, ?, ?, NULL, ?, ?, 'cash', 'product', 0, ?, ?)`,
		payID, f.gymID, now, now, deletedAt, "P-"+payID.String()[:8], amountCents, amountCents, paymentDate, f.operatorID); err != nil {
		t.Fatalf("seed product payment: %v", err)
	}
	saleID := uuid.New()
	if _, err := f.db.Exec(`
		INSERT INTO sales (id, gym_id, version, created_at, updated_at, deleted_at, payment_id, subtotal, discount, total)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, 0, ?)`,
		saleID, f.gymID, now, now, deletedAt, payID, amountCents, amountCents); err != nil {
		t.Fatalf("seed sale: %v", err)
	}
	prodID := uuid.New()
	if _, err := f.db.Exec(`
		INSERT INTO products (id, gym_id, version, created_at, updated_at, name, price, stock)
		VALUES (?, ?, 1, ?, ?, ?, 1000, 100)`,
		prodID, f.gymID, now, now, "Prod "+prodID.String()[:8]); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	for _, q := range quantities {
		itemID := uuid.New()
		if _, err := f.db.Exec(`
			INSERT INTO sale_items (id, gym_id, version, created_at, updated_at, deleted_at, sale_id, product_id, product_name_snapshot, unit_price_snapshot, quantity, line_total)
			VALUES (?, ?, 1, ?, ?, ?, ?, ?, 'Prod', 1000, ?, ?)`,
			itemID, f.gymID, now, now, deletedAt, saleID, prodID, q, int64(q)*1000); err != nil {
			t.Fatalf("seed sale_item: %v", err)
		}
	}
}

// TestSumProductSalesBetween_DineroYUnidades — el $ sale de payments
// concept='product' (mismo criterio cash-based por payment_date que
// SumPaymentsBetween, para que el KPI cuadre contra Ingresos) y las
// unidades de sale_items. Membership, soft-deleted y fuera de
// ventana no cuentan.
func TestSumProductSalesBetween_DineroYUnidades(t *testing.T) {
	f := setupReaderDB(t)

	f.seedProductSale(t, 10000, "2026-07-10", []int{3, 2}, false) // $100.00, 5 uds
	f.seedProductSale(t, 5025, "2026-07-20", []int{1}, false)     // $50.25, 1 ud
	f.seedProductSale(t, 9900, "2026-06-01", []int{7}, false)     // fuera de ventana
	f.seedProductSale(t, 7700, "2026-07-15", []int{4}, true)      // soft-deleted
	ana := f.seedMember(t, "Ana")
	f.seedPayment(t, &ana, "membership", 0, "2026-07-12", false) // otro concepto

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	got, err := reportsInfra.NewSQLiteReader().SumProductSalesBetween(tx, f.gymID,
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SumProductSalesBetween: %v", err)
	}
	if got.Amount != 150.25 {
		t.Errorf("amount = %v, want 150.25", got.Amount)
	}
	if got.Units != 6 {
		t.Errorf("units = %d, want 6", got.Units)
	}
}

func TestTopProducts_UsesGrossSalesWhileRefundsRemainSeparate(t *testing.T) {
	f := setupReaderDB(t)
	f.seedProductSale(t, 2000, "2026-07-10", []int{2}, false)
	var rootPaymentID, saleID, saleItemID string
	if err := f.db.QueryRowx(`SELECT p.id,s.id,si.id
		FROM payments p JOIN sales s ON s.payment_id=p.id
		JOIN sale_items si ON si.sale_id=s.id
		WHERE p.gym_id=? AND p.concept='product'`, f.gymID.String()).
		Scan(&rootPaymentID, &saleID, &saleItemID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixMilli()
	refundID := uuid.New().String()
	if _, err := f.db.Exec(`INSERT INTO refunds
		(id,gym_id,version,created_at,updated_at,root_payment_id,sale_id,amount,method,refunded_on,
		 reason,kind,balance_cancelled,legacy_incomplete,idempotency_key,idempotency_fingerprint,created_by)
		VALUES(?,?,1,?,?,?,?,1000,'cash','2026-07-11','devolución parcial','revenue_refund',0,0,?,?,?)`,
		refundID, f.gymID.String(), now, now, rootPaymentID, saleID, "refund:top-products", "fingerprint", f.operatorID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO refund_items
		(id,gym_id,version,created_at,updated_at,refund_id,sale_item_id,quantity,amount,disposition)
		VALUES(?,?,1,?,?,?,?,1,1000,'returned_to_stock')`,
		uuid.New().String(), f.gymID.String(), now, now, refundID, saleItemID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := reportsInfra.NewSQLiteReader().TopProductsBetween(tx, f.gymID,
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Quantity != 2 || rows[0].Revenue != 20 {
		t.Fatalf("top products=%+v; want gross 2 units/$20 independent of the separate $10 refund", rows)
	}
}

// TestListRecentPayments_TraeResumenDeVenta — el bug de Cobros: una venta
// walk-in (member_id NULL) se pintaba como "—" porque el widget sólo traía
// member_name. Ahora los pagos de producto viajan con el resumen de la
// venta; los de membresía no llevan resumen.
func TestListRecentPayments_TraeResumenDeVenta(t *testing.T) {
	f := setupReaderDB(t)
	f.seedProductSale(t, 10000, "2026-07-10", []int{3}, false)
	ana := f.seedMember(t, "Ana")
	f.seedPayment(t, &ana, "membership", 0, "2026-07-12", false)

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := reportsInfra.NewSQLiteReader().ListRecentPayments(tx, f.gymID, 10)
	if err != nil {
		t.Fatalf("ListRecentPayments: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Orden: membership (12-jul) primero, la venta (10-jul) después.
	if rows[0].SaleSummary != nil {
		t.Errorf("membership no debe traer resumen: %v", *rows[0].SaleSummary)
	}
	if rows[1].SaleSummary == nil || *rows[1].SaleSummary != "Prod ×3" {
		t.Errorf("venta sin resumen esperado 'Prod ×3': %+v", rows[1].SaleSummary)
	}
	if rows[1].MemberName != nil {
		t.Errorf("venta walk-in no debe traer member_name")
	}
}

func TestRecoverable_ExcludesRenewedCoverageAndReturnsOneRowPerMember(t *testing.T) {
	f := setupReaderDB(t)
	today := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)

	renewed := f.seedMember(t, "Renovada")
	f.seedMembership(t, renewed, "2026-07-01", "2026-08-01", "replaced")
	f.seedMembership(t, renewed, "2026-08-01", "2026-09-01", "active")

	recoverable := f.seedMember(t, "Recuperable")
	f.seedMembership(t, recoverable, "2026-06-01", "2026-07-20", "active")
	// Otra vigencia histórica vencida no debe duplicar al socio.
	f.seedMembership(t, recoverable, "2026-04-01", "2026-05-01", "replaced")

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	reader := reportsInfra.NewSQLiteReader()
	count, err := reader.CountExpiredRecoverable(tx, f.gymID, today, 60)
	if err != nil {
		t.Fatalf("count recoverable: %v", err)
	}
	rows, err := reader.ListExpiredRecoverable(tx, f.gymID, today, 60, 3)
	if err != nil {
		t.Fatalf("list recoverable: %v", err)
	}
	if count != 1 || len(rows) != 1 || rows[0].MemberID != recoverable {
		t.Errorf("count/rows = %d/%+v, want only Recuperable", count, rows)
	}
}

func TestInactiveInvoluntary_RequiresCurrentCoverage(t *testing.T) {
	f := setupReaderDB(t)
	today := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)

	covered := f.seedMember(t, "Cubierta")
	f.seedMembership(t, covered, "2026-08-01", "2026-09-01", "active")
	uncovered := f.seedMember(t, "Sin membresía")
	expired := f.seedMember(t, "Vencida")
	f.seedMembership(t, expired, "2026-06-01", "2026-07-01", "active")
	future := f.seedMember(t, "Futura")
	f.seedMembership(t, future, "2026-08-20", "2026-09-20", "active")

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := reportsInfra.NewSQLiteReader().ListInactiveInvoluntary(tx, f.gymID, today, 14)
	if err != nil {
		t.Fatalf("inactive: %v", err)
	}
	if len(rows) != 1 || rows[0].MemberID != covered {
		t.Errorf("inactive rows = %+v, want only covered member; uncovered=%s", rows, uncovered)
	}
}

func TestCurrentCoverage_EarlyRenewalCountsMemberOnceAndExcludesFutureOnly(t *testing.T) {
	f := setupReaderDB(t)
	today := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

	// En una renovación anticipada la vigencia que cubre hoy queda replaced
	// y la nueva active empieza justo en su expiración. Ambas tocan el día de
	// frontera, pero el socio debe contarse una sola vez.
	renewed := f.seedMember(t, "Renovada anticipadamente")
	f.seedMembership(t, renewed, "2026-07-20", "2026-08-20", "replaced")
	f.seedMembership(t, renewed, "2026-08-20", "2026-09-20", "active")

	// Una membresía pagada/programada que todavía no empieza no es cobertura
	// actual ni debe aparecer como por vencer.
	future := f.seedMember(t, "Empieza mañana")
	f.seedMembership(t, future, "2026-08-21", "2026-08-25", "active")

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	reader := reportsInfra.NewSQLiteReader()
	active, err := reader.CountActiveMembers(tx, f.gymID, today)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	byType, err := reader.ActiveMembersByType(tx, f.gymID, today)
	if err != nil {
		t.Fatalf("active by type: %v", err)
	}
	gender, err := reader.GenderComposition(tx, f.gymID, today)
	if err != nil {
		t.Fatalf("gender composition: %v", err)
	}
	expiring, err := reader.CountExpiringBetween(tx, f.gymID, today, today.AddDate(0, 0, 7))
	if err != nil {
		t.Fatalf("expiring: %v", err)
	}

	if active != 1 || byType["Mensual"] != 1 || gender.Total != 1 {
		t.Errorf("active/byType/gender = %d/%v/%d, want one covered member", active, byType, gender.Total)
	}
	if expiring != 0 {
		t.Errorf("expiring = %d, future-only membership must not count", expiring)
	}
}

// Los agregados SQLite SUM(...) devuelven NULL cuando el gimnasio todavía no
// tiene productos o socios activos. Los reportes deben representar ese estado
// vacío con contadores en cero, no fallar al escanear NULL en un int.
func TestEmptyGym_CountAggregatesReturnZero(t *testing.T) {
	f := setupReaderDB(t)
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	reader := reportsInfra.NewSQLiteReader()

	stock, err := reader.CountCriticalStock(tx, f.gymID)
	if err != nil {
		t.Fatalf("critical stock on empty gym: %v", err)
	}
	if stock.OutCount != 0 || stock.LowCount != 0 {
		t.Fatalf("critical stock = %+v, want zero counts", stock)
	}

	gender, err := reader.GenderComposition(tx, f.gymID, time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("gender composition on empty gym: %v", err)
	}
	if gender.Hombre != 0 || gender.Mujer != 0 || gender.NoEspecificado != 0 || gender.Total != 0 {
		t.Fatalf("gender composition = %+v, want zero counts", gender)
	}
}
