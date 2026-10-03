//go:build server && integration

package repositories_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	cash "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	repositories "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

func TestPostgresCashSessionConstraintsAndNullableDiscrepancy(t *testing.T) {
	db := testutil.OpenPostgres(t)
	gymID, userID := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO gyms(id,gym_id,version,created_at,updated_at,name)
		VALUES(?,?,1,NOW(),NOW(),'Cash constraints')`, gymID, gymID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO users(id,gym_id,version,created_at,updated_at,email,password_hash,full_name,role)
		VALUES(?,?,1,NOW(),NOW(),?,'x','Owner','owner')`, userID, gymID,
		"cash-"+userID.String()[:8]+"@t.mx").Error; err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	invalidID := uuid.New()
	if err := db.Exec(`INSERT INTO cash_close_events(
		id,gym_id,version,created_at,updated_at,close_date,calculated_cash,closed_by,
		drawer_id,drawer_code,operational_date,sequence,status,opening_cash,
		opening_cash_known,activity_cash,adjusted_after_withdrawal,opened_at,opened_by)
		VALUES(?,?,1,NOW(),NOW(),?,0,?,?,'main',?,1,'invented',0,TRUE,0,FALSE,NOW(),?)`,
		invalidID, gymID, day, userID, gymID, day, userID).Error; err == nil {
		t.Fatal("Postgres accepted an invalid cash-session status")
	}

	sessionID := uuid.New()
	if err := db.Exec(`INSERT INTO cash_close_events(
		id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
		drawer_id,drawer_code,operational_date,sequence,status,opening_cash,
		opening_cash_known,activity_cash,cash_left,withdrawn_cash,withdrawal_destination,
		adjusted_after_withdrawal,integrity_note,opened_at,opened_by,closed_at,reconciled_at,
		reconciled_by,withdrawn_at,withdrawn_by)
		VALUES(?,?,1,NOW(),NOW(),?,90,100,?,?,'main',?,1,'withdrawn',0,
		TRUE,90,10,90,'gym_fund',TRUE,'registro corregido después del retiro',
		NOW(),?,NOW(),NOW(),?,NOW(),?)`, sessionID, gymID, day, userID, gymID, day,
		userID, userID, userID).Error; err != nil {
		t.Fatalf("insert valid adjusted withdrawal: %v", err)
	}
	var discrepancy sql.NullFloat64
	if err := db.Raw(`SELECT discrepancy FROM cash_close_events WHERE id=?`, sessionID).Scan(&discrepancy).Error; err != nil {
		t.Fatal(err)
	}
	if discrepancy.Valid {
		t.Fatalf("adjusted withdrawn discrepancy=%v, want NULL", discrepancy.Float64)
	}
}

func TestPostgresCashCoverageDerivesPostWithdrawalCorrectionFromCurrentMath(t *testing.T) {
	db := testutil.OpenPostgres(t)
	gymID, userID, paymentID, sessionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	day := base.Format("2006-01-02")
	if err := db.Exec(`INSERT INTO gyms(id,gym_id,version,created_at,updated_at,name)
		VALUES(?,?,1,?,?,'Cash correction coverage')`, gymID, gymID, base, base).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO users(id,gym_id,version,created_at,updated_at,email,password_hash,full_name,role)
		VALUES(?,?,1,?,?,?,'x','Owner','owner')`, userID, gymID, base, base,
		"cash-coverage-"+userID.String()[:8]+"@t.mx").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,
		cash_drawer_id,concept,balance_pending,payment_date,operator_id)
		VALUES(?,?,1,?,?,?,20,20,'cash',?,'other',0,?,?)`,
		paymentID, gymID, base, base, "P-"+paymentID.String()[:8], gymID, day, userID).Error; err != nil {
		t.Fatal(err)
	}
	closedAt, withdrawnAt := base.Add(time.Minute), base.Add(2*time.Minute)
	if err := db.Exec(`INSERT INTO cash_close_events(
		id,gym_id,version,created_at,updated_at,close_date,calculated_cash,counted_cash,closed_by,
		drawer_id,drawer_code,operational_date,sequence,status,opening_cash,opening_cash_known,
		activity_cash,cash_left,withdrawn_cash,withdrawal_destination,adjusted_after_withdrawal,
		opened_at,opened_by,closed_at,reconciled_at,reconciled_by,withdrawn_at,withdrawn_by)
		VALUES(?,?,1,?,?,?,20,20,?,?,'main',?,1,'withdrawn',0,TRUE,20,0,20,'gym_fund',FALSE,
		?,?,?, ?,?,?,?)`, sessionID, gymID, base, withdrawnAt, day, userID, gymID, day,
		base.Add(-time.Minute), userID, closedAt, closedAt, userID, withdrawnAt, userID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE payments SET amount=40,recognized_amount=40,version=2,updated_at=? WHERE id=?`,
		withdrawnAt.Add(time.Minute), paymentID).Error; err != nil {
		t.Fatal(err)
	}

	uow := sharedDomain.NewPostgresUnitOfWork(db)
	tx, err := uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := repositories.NewCashClosePostgresReader().SessionCoverage(tx,
		billingRepo.CashSessionCoverageQuery{GymID: gymID, From: base, To: base})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Complete || coverage.WithdrawnSessions != 1 ||
		coverage.AdjustedAfterWithdrawalSessions != 1 || coverage.UncoveredActivityDays != 1 {
		t.Fatalf("coverage=%+v; transfer must stay withdrawn while its old difference is uncertified", coverage)
	}
}

func TestPostgresCountedPeriodWithoutWithdrawalScopesCoverageAndOpening(t *testing.T) {
	db := testutil.OpenPostgres(t)
	ctx := context.Background()
	gymID, userID := uuid.New(), uuid.New()
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	day := start.Format("2006-01-02")
	if err := db.Exec(`INSERT INTO gyms(id,gym_id,version,created_at,updated_at,name) VALUES(?,?,1,?,?,'Counted periods')`, gymID, gymID, start, start).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO users(id,gym_id,version,created_at,updated_at,email,password_hash,full_name,role) VALUES(?,?,1,?,?,?,'x','Owner','owner')`, userID, gymID, start, start, "period-"+userID.String()+"@t.mx").Error; err != nil {
		t.Fatal(err)
	}
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	events := repositories.NewCashCloseEventPostgresRepository()
	reader := repositories.NewCashClosePostgresReader()
	session, err := cash.Open(gymID, gymID, userID, start, 1, 100, start)
	if err != nil {
		t.Fatal(err)
	}
	closeAt := start.Add(time.Minute)
	if err = session.Close(20, userID, closeAt); err != nil {
		t.Fatal(err)
	}
	if err = session.Reconcile(120, nil, userID, closeAt); err != nil {
		t.Fatal(err)
	}
	if err = session.Finish(closeAt); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(ctx, func(tx sharedDomain.Transaction) error { _, err := events.Create(tx, session); return err }); err != nil {
		t.Fatal(err)
	}
	for i, amount := range []int{20, 10} {
		id := uuid.New()
		at := start.Add(time.Duration(i*2) * time.Minute)
		if err := db.Exec(`INSERT INTO payments(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,cash_drawer_id,concept,balance_pending,payment_date,operator_id) VALUES(?,?,1,?,?,?,?,?,'cash',?,'other',0,?,?)`, id, gymID, at, at, "P-"+id.String()[:8], amount, amount, gymID, day, userID).Error; err != nil {
			t.Fatal(err)
		}
	}
	tx, _ := uow.Query(ctx)
	coverage, err := reader.SessionCoverage(tx, billingRepo.CashSessionCoverageQuery{GymID: gymID, From: start, To: start})
	if err != nil || coverage.Complete || coverage.UncoveredActivityDays != 1 {
		t.Fatalf("coverage=%+v err=%v", coverage, err)
	}
	stored, err := events.GetByID(tx, gymID, session.ID)
	if err != nil || stored.FinishedAt() == nil || stored.WithdrawnAt != nil || *stored.CashLeft != 120 {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	entries, err := reader.CashEntries(tx, billingRepo.CashCloseQuery{GymID: gymID, DrawerID: gymID, Date: start})
	if err != nil || len(entries) != 2 || entries[0].Amount != 10 || entries[1].Amount != 20 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	previous, err := events.LatestBefore(tx, gymID, gymID, start.AddDate(0, 0, 1))
	if err != nil || previous.ID != session.ID {
		t.Fatalf("opening source=%+v err=%v", previous, err)
	}
}
