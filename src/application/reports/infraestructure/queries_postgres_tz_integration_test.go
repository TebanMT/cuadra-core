//go:build server && integration

package infraestructure_test

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// Frontera del día local del gym en Postgres.
//
// DOS motivos para que esto sea un test de INTEGRACIÓN y no unitario:
//
//  1. Semántica: verifica que los check-ins de la tarde-noche (el horario
//     PICO del gym) caigan en su día y no en el siguiente. Antes se
//     filtraba y agrupaba por día UTC, y en CDMX la medianoche UTC son las
//     6 PM.
//
//  2. Tipado de parámetros: `AT TIME ZONE ?` mete un parámetro BINDEADO
//     dentro de una expresión SQL — exactamente la clase de query que tumbó
//     los recordatorios 3 días en julio-2026 (`::date - ?` resolvía al
//     operador equivocado y reventaba AL PLANEAR, sin importar los datos).
//     Aquello no se reprodujo con literales en psql. Regla de la casa desde
//     entonces: se prueba A TRAVÉS del driver. Eso hace este archivo.
//
// Run:
//
//	go test -tags 'server integration' ./src/application/reports/...
const (
	cdmxTZ = "America/Mexico_City"
	// 27-jul-2026 7:30 PM en CDMX — en UTC ya es el 28.
	nocheDel27 = "2026-07-28T01:30:00Z"
	// 27-jul-2026 9:00 AM en CDMX — mismo día en ambas zonas.
	mananaDel27 = "2026-07-27T15:00:00Z"
)

type tzFixture struct {
	db       *gorm.DB
	uow      sharedDomain.UnitOfWork
	gymID    uuid.UUID
	userID   uuid.UUID
	memberID uuid.UUID
}

func setupTZFixture(t *testing.T) *tzFixture {
	t.Helper()
	db := testutil.OpenPostgres(t)
	f := &tzFixture{
		db:       db,
		uow:      sharedDomain.NewPostgresUnitOfWork(db),
		gymID:    uuid.New(),
		userID:   uuid.New(),
		memberID: uuid.New(),
	}
	if err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone)
		VALUES (?, ?, 1, NOW(), NOW(), 'TZ Gym', 'MX', 'America/Mexico_City')`,
		f.gymID, f.gymID).Error; err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 'x', 'Operador', 'operator')`,
		f.userID, f.gymID, "op-"+f.userID.String()[:8]+"@t.mx").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO members (id, gym_id, version, created_at, updated_at, folio, full_name, phone, created_by)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 'Ana', '5512345678', ?)`,
		f.memberID, f.gymID, "SOC-"+f.memberID.String()[:8], f.userID).Error; err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return f
}

func (f *tzFixture) seedCheckin(t *testing.T, iso string) {
	t.Helper()
	at, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Fatalf("parse %q: %v", iso, err)
	}
	if err := f.db.Exec(`
		INSERT INTO checkins (id, gym_id, version, created_at, updated_at, member_id, checkin_at, method, result, operator_id)
		VALUES (?, ?, 1, NOW(), NOW(), ?, ?, 'manual', 'allowed_active', ?)`,
		uuid.New(), f.gymID, f.memberID, at, f.userID).Error; err != nil {
		t.Fatalf("seed checkin: %v", err)
	}
}

func day(t *testing.T, ymd string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", ymd)
	if err != nil {
		t.Fatalf("parse day %q: %v", ymd, err)
	}
	return d
}

func TestPG_CountCheckins_NocheLocalCuentaEnSuDia(t *testing.T) {
	f := setupTZFixture(t)
	f.seedCheckin(t, nocheDel27)

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	reader := reportsInfra.NewPostgresReader()

	n, err := reader.CountCheckinsBetween(tx, f.gymID, cdmxTZ, day(t, "2026-07-27"), day(t, "2026-07-27"))
	if err != nil {
		t.Fatalf("CountCheckinsBetween: %v", err)
	}
	if n != 1 {
		t.Errorf("check-ins del 27 (día local) = %d, want 1", n)
	}

	n, err = reader.CountCheckinsBetween(tx, f.gymID, cdmxTZ, day(t, "2026-07-28"), day(t, "2026-07-28"))
	if err != nil {
		t.Fatalf("CountCheckinsBetween: %v", err)
	}
	if n != 0 {
		t.Errorf("check-ins del 28 (día local) = %d, want 0 — el gym no había abierto", n)
	}
}

// TestPG_CheckinsDailySeries_AgrupaPorDiaLocal — además de la semántica,
// este es EL test que ejercita `AT TIME ZONE ?` con parámetro bindeado.
// Si Postgres no pudiera inferir el tipo del parámetro, reventaría aquí al
// planear, igual que el incidente de julio.
func TestPG_CheckinsDailySeries_AgrupaPorDiaLocal(t *testing.T) {
	f := setupTZFixture(t)
	f.seedCheckin(t, mananaDel27)
	f.seedCheckin(t, nocheDel27)

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := reportsInfra.NewPostgresReader().
		CheckinsDailySeries(tx, f.gymID, cdmxTZ, day(t, "2026-07-27"), day(t, "2026-07-27"))
	if err != nil {
		t.Fatalf("CheckinsDailySeries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("series = %+v, want 1 barra (ambos check-ins son del 27 local)", rows)
	}
	if got := rows[0].Date.Format("2006-01-02"); got != "2026-07-27" {
		t.Errorf("barra en %s, want 2026-07-27", got)
	}
	if rows[0].Count != 2 {
		t.Errorf("count = %d, want 2", rows[0].Count)
	}
}

// TestPG_ZonaInvalidaNoRevientaElQuery — un timezone basura en la config del
// gym degrada a UTC (tz.NameOrUTC), nunca tumba la página de reportes.
// Postgres resuelve zonas con SU catálogo, así que sin ese filtro previo un
// nombre inválido sería un error de query, no un fallback.
func TestPG_ZonaInvalidaNoRevientaElQuery(t *testing.T) {
	f := setupTZFixture(t)
	f.seedCheckin(t, mananaDel27)

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := reportsInfra.NewPostgresReader().
		CheckinsDailySeries(tx, f.gymID, "No/Existe", day(t, "2026-07-27"), day(t, "2026-07-27"))
	if err != nil {
		t.Fatalf("una zona inválida no debe romper el query: %v", err)
	}
	if len(rows) != 1 || rows[0].Count != 1 {
		t.Errorf("series con zona inválida = %+v, want 1 barra con 1", rows)
	}
}

// TestPG_ExpensesDailySeries_UsaLaFechaDePagoDeLaCompra — un movimiento de
// stock, por sí solo, no representa una salida de dinero. La serie suma la
// compra explícita cuando se paga y la ubica en paid_on, igual que el total
// canónico del período.
func TestPG_ExpensesDailySeries_UsaLaFechaDePagoDeLaCompra(t *testing.T) {
	f := setupTZFixture(t)
	productID := uuid.New()
	if err := f.db.Exec(`
		INSERT INTO products (id, gym_id, version, created_at, updated_at, name, price, stock)
		VALUES (?, ?, 1, NOW(), NOW(), 'Proteína', 500, 10)`,
		productID, f.gymID).Error; err != nil {
		t.Fatalf("seed product: %v", err)
	}
	at, _ := time.Parse(time.RFC3339, nocheDel27)
	movementID := uuid.New()
	if err := f.db.Exec(`
		INSERT INTO stock_movements (id, gym_id, version, created_at, updated_at, product_id, movement_type, delta, cost, operator_id)
		VALUES (?, ?, 1, ?, ?, ?, 'restock', 10, 200, ?)`,
		movementID, f.gymID, at, at, productID, f.userID).Error; err != nil {
		t.Fatalf("seed stock movement: %v", err)
	}
	if err := f.db.Exec(`
		INSERT INTO inventory_purchases
			(id, gym_id, version, created_at, updated_at, stock_movement_id, product_id,
			 quantity, unit_cost, total_amount, status, paid_on, payment_method, paid_from,
			 idempotency_key, created_by)
		VALUES (?, ?, 1, ?, ?, ?, ?, 10, 200, 2000, 'paid', '2026-07-27',
			'transfer', 'gym_fund', ?, ?)`,
		uuid.New(), f.gymID, at, at, movementID, productID,
		"daily-series-explicit-purchase", f.userID).Error; err != nil {
		t.Fatalf("seed inventory purchase: %v", err)
	}

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := reportsInfra.NewPostgresReader().
		ExpensesDailySeries(tx, f.gymID, cdmxTZ, day(t, "2026-07-27"), day(t, "2026-07-27"))
	if err != nil {
		t.Fatalf("ExpensesDailySeries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("series = %+v, want 1 barra en el 27 local", rows)
	}
	if got := rows[0].Date.Format("2006-01-02"); got != "2026-07-27" {
		t.Errorf("barra en %s, want 2026-07-27", got)
	}
	if rows[0].Total != 2000 {
		t.Errorf("total = %.2f, want 2000.00", rows[0].Total)
	}
}

func TestPG_CanonicalFinancialEquationIsCentSafe(t *testing.T) {
	f := setupTZFixture(t)
	day := day(t, "2026-08-24")
	for i, amount := range []string{"0.10", "0.20"} {
		id := uuid.New()
		if err := f.db.Exec(`INSERT INTO payments
			(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,
			 concept,balance_pending,payment_date,operator_id)
			VALUES(?,?,1,NOW(),NOW(),?,?::numeric,?::numeric,'transfer','other',0,?,?)`,
			id, f.gymID, "CENT-"+string(rune('A'+i)), amount, amount, day.Format("2006-01-02"), f.userID).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.Exec(`INSERT INTO expenses
		(id,gym_id,version,created_at,updated_at,expense_date,amount,category,payment_method,paid_from,created_by)
		VALUES(?,?,1,NOW(),NOW(),?,0.10,'otros','transfer','gym_fund',?)`,
		uuid.New(), f.gymID, day.Format("2006-01-02"), f.userID).Error; err != nil {
		t.Fatal(err)
	}
	productID, movementID := uuid.New(), uuid.New()
	if err := f.db.Exec(`INSERT INTO products
		(id,gym_id,version,created_at,updated_at,name,price,stock)
		VALUES(?,?,1,NOW(),NOW(),'Centavos',1,1)`, productID, f.gymID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec(`INSERT INTO stock_movements
		(id,gym_id,version,created_at,updated_at,product_id,movement_type,delta,cost,operator_id,is_purchase)
		VALUES(?,?,1,NOW(),NOW(),?,'restock',1,0.20,?,TRUE)`, movementID, f.gymID, productID, f.userID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec(`INSERT INTO inventory_purchases
		(id,gym_id,version,created_at,updated_at,stock_movement_id,product_id,quantity,unit_cost,total_amount,
		 status,paid_on,payment_method,paid_from,idempotency_key,created_by)
		VALUES(?,?,1,NOW(),NOW(),?,?,1,0.20,0.20,'paid',?,'transfer','gym_fund',?,?)`,
		uuid.New(), f.gymID, movementID, productID, day.Format("2006-01-02"), "cent-safe-purchase", f.userID).Error; err != nil {
		t.Fatal(err)
	}
	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reportsInfra.NewPostgresReader().CanonicalFinancialBetween(tx, f.gymID, cdmxTZ, day, day)
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

// TestPG_CanonicalFinancialEquationMatchesThe415Scenario protects the exact
// example that exposed the old hidden-$5 bug. The product purchase is a paid
// inventory purchase (not COGS and not a generic Expense), so every peso has
// one visible home in the canonical equation.
func TestPG_CanonicalFinancialEquationMatchesThe415Scenario(t *testing.T) {
	f := setupTZFixture(t)
	reportDay := day(t, "2026-08-23")

	for _, payment := range []struct {
		amount  string
		concept string
		member  any
		folio   string
	}{
		{amount: "400.00", concept: "membership", member: f.memberID, folio: "GOLDEN-MEMBERSHIP"},
		{amount: "15.00", concept: "product", member: nil, folio: "GOLDEN-PRODUCT"},
	} {
		if err := f.db.Exec(`INSERT INTO payments
			(id,gym_id,version,created_at,updated_at,folio,member_id,amount,recognized_amount,
			 payment_method,concept,balance_pending,payment_date,operator_id)
			VALUES(?,?,1,NOW(),NOW(),?,?,?::numeric,?::numeric,'transfer',?,0,?,?)`,
			uuid.New(), f.gymID, payment.folio, payment.member, payment.amount, payment.amount,
			payment.concept, reportDay.Format("2006-01-02"), f.userID).Error; err != nil {
			t.Fatalf("seed %s payment: %v", payment.concept, err)
		}
	}

	if err := f.db.Exec(`INSERT INTO expenses
		(id,gym_id,version,created_at,updated_at,expense_date,amount,category,payment_method,paid_from,created_by)
		VALUES(?,?,1,NOW(),NOW(),?,1500.00,'renta','transfer','gym_fund',?)`,
		uuid.New(), f.gymID, reportDay.Format("2006-01-02"), f.userID).Error; err != nil {
		t.Fatalf("seed expense: %v", err)
	}

	productID, movementID := uuid.New(), uuid.New()
	if err := f.db.Exec(`INSERT INTO products
		(id,gym_id,version,created_at,updated_at,name,price,stock)
		VALUES(?,?,1,NOW(),NOW(),'Compra golden',15.00,10)`, productID, f.gymID).Error; err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if err := f.db.Exec(`INSERT INTO stock_movements
		(id,gym_id,version,created_at,updated_at,product_id,movement_type,delta,cost,operator_id,is_purchase)
		VALUES(?,?,1,NOW(),NOW(),?,'restock',10,5.00,?,TRUE)`,
		movementID, f.gymID, productID, f.userID).Error; err != nil {
		t.Fatalf("seed purchase stock movement: %v", err)
	}
	if err := f.db.Exec(`INSERT INTO inventory_purchases
		(id,gym_id,version,created_at,updated_at,stock_movement_id,product_id,quantity,unit_cost,total_amount,
		 status,paid_on,payment_method,paid_from,idempotency_key,created_by)
		VALUES(?,?,1,NOW(),NOW(),?,?,10,5.00,50.00,'paid',?,'transfer','gym_fund',?,?)`,
		uuid.New(), f.gymID, movementID, productID, reportDay.Format("2006-01-02"),
		"golden-415-purchase", f.userID).Error; err != nil {
		t.Fatalf("seed paid inventory purchase: %v", err)
	}

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reportsInfra.NewPostgresReader().CanonicalFinancialBetween(
		tx, f.gymID, cdmxTZ, reportDay, reportDay,
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

func TestPG_LegacyPurchaseWithKnownCostIsCompleteAndCounted(t *testing.T) {
	f := setupTZFixture(t)
	at := time.Date(2026, 8, 22, 18, 0, 0, 0, time.UTC) // mediodía en CDMX
	productID := uuid.New()
	if err := f.db.Exec(`INSERT INTO products
		(id,gym_id,version,created_at,updated_at,name,price,stock)
		VALUES(?,?,1,?,?,'Agua legacy',15.00,13)`,
		productID, f.gymID, at, at).Error; err != nil {
		t.Fatalf("seed product: %v", err)
	}
	knownID, unknownID, captureID := uuid.New(), uuid.New(), uuid.New()
	for _, movement := range []struct {
		id         uuid.UUID
		at         time.Time
		delta      int
		cost       any
		isPurchase bool
	}{
		{id: knownID, at: at, delta: 10, cost: "5.00", isPurchase: true},
		{id: unknownID, at: at.Add(time.Minute), delta: 3, cost: nil, isPurchase: true},
		{id: captureID, at: at.Add(2 * time.Minute), delta: 7, cost: "2.50", isPurchase: false},
	} {
		if err := f.db.Exec(`INSERT INTO stock_movements
			(id,gym_id,version,created_at,updated_at,product_id,movement_type,delta,cost,operator_id,is_purchase)
			VALUES(?,?,1,?,?,?,'restock',?,?,?,?)`,
			movement.id, f.gymID, movement.at, movement.at, productID,
			movement.delta, movement.cost, f.userID, movement.isPurchase).Error; err != nil {
			t.Fatalf("seed legacy stock movement: %v", err)
		}
	}
	// A zero-valued legacy placeholder must not hide the exact amount that
	// remains recoverable from the stock journal.
	if err := f.db.Exec(`INSERT INTO inventory_purchases
		(id,gym_id,version,created_at,updated_at,stock_movement_id,product_id,quantity,
		 unit_cost,total_amount,status,idempotency_key,created_by)
		VALUES(?,?,1,?,?,?,?,10,0,0,'legacy_incomplete',?,?)`,
		uuid.New(), f.gymID, at, at, knownID, productID,
		"legacy-known-cost", f.userID).Error; err != nil {
		t.Fatalf("seed legacy purchase placeholder: %v", err)
	}

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reader := reportsInfra.NewPostgresReader()
	reportDay := day(t, "2026-08-22")
	total, err := reader.SumInventoryCostBetween(tx, f.gymID, cdmxTZ, reportDay, reportDay)
	if err != nil {
		t.Fatal(err)
	}
	if total != 50 {
		t.Fatalf("legacy purchase total=%v, want 50", total)
	}
	canonical, err := reader.CanonicalFinancialBetween(tx, f.gymID, cdmxTZ, reportDay, reportDay)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.InventoryPurchases != 50 || canonical.LegacyPurchaseCount != 1 {
		t.Fatalf("canonical=%+v, want purchases=50 and one truly missing amount", canonical)
	}
	series, err := reader.ExpensesDailySeries(tx, f.gymID, cdmxTZ, reportDay, reportDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Date.Format("2006-01-02") != "2026-08-22" || series[0].Total != 50 {
		t.Fatalf("expense series=%+v, want 2026-08-22/$50", series)
	}
	rows, err := reader.ListInventoryCostsBetween(tx, f.gymID, cdmxTZ, reportDay, reportDay, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MovementID != knownID || rows[0].CostUnit != 5 || rows[0].CostTotal != 50 {
		t.Fatalf("legacy purchase detail=%+v", rows)
	}
}

func TestPG_LegacyCashExpenseIsVisibleButNeverAssumedPhysical(t *testing.T) {
	f := setupTZFixture(t)
	reportDay := day(t, "2026-08-24")
	expenseID := uuid.New()
	if err := f.db.Exec(`INSERT INTO expenses
		(id,gym_id,version,created_at,updated_at,expense_date,amount,category,payment_method,paid_from,created_by)
		VALUES(?,?,1,NOW(),NOW(),?,50.00,'otros','cash','cash_register',?)`,
		expenseID, f.gymID, reportDay.Format("2006-01-02"), f.userID).Error; err != nil {
		t.Fatal(err)
	}

	read := func() (canonicalCount, coverageCount int, activity float64) {
		t.Helper()
		tx, err := f.uow.Query(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		reader := reportsInfra.NewPostgresReader()
		canonical, err := reader.CanonicalFinancialBetween(tx, f.gymID, cdmxTZ, reportDay, reportDay)
		if err != nil {
			t.Fatal(err)
		}
		coverage, err := reader.SumCashCountedBetween(tx, f.gymID, reportDay, reportDay)
		if err != nil {
			t.Fatal(err)
		}
		activity, err = reader.SumCashClosedBetween(tx, f.gymID, reportDay, reportDay)
		if err != nil {
			t.Fatal(err)
		}
		if canonical.OperatingExpenses != 50 {
			t.Fatalf("operating expenses=%.2f, want 50 even while physical source is unknown", canonical.OperatingExpenses)
		}
		return canonical.LegacyCashSourceUnverifiedCount, coverage.LegacyCashSourceUnverifiedCount, activity
	}

	if canonicalCount, coverageCount, activity := read(); canonicalCount != 1 || coverageCount != 1 || activity != 0 {
		t.Fatalf("before repair canonical/coverage/activity=%d/%d/%.2f, want 1/1/0", canonicalCount, coverageCount, activity)
	}

	movementID := uuid.New()
	if err := f.db.Exec(`INSERT INTO cash_movements
		(id,gym_id,version,created_at,updated_at,movement_on,amount,movement_type,reason,operator_id,expense_id,classification_status,cash_drawer_id)
		VALUES(?,?,1,NOW(),NOW(),?,50.00,'cash_out','Origen histórico confirmado',?,?,'expense',?)`,
		movementID, f.gymID, reportDay.Format("2006-01-02"), f.userID, expenseID, f.gymID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec(`UPDATE expenses SET cash_movement_id=?,updated_at=NOW() WHERE id=?`, movementID, expenseID).Error; err != nil {
		t.Fatal(err)
	}

	if canonicalCount, coverageCount, activity := read(); canonicalCount != 0 || coverageCount != 0 || activity != -50 {
		t.Fatalf("after Caja repair canonical/coverage/activity=%d/%d/%.2f, want 0/0/-50", canonicalCount, coverageCount, activity)
	}
}

func TestPG_ProductProfitabilityIsCentExactAcrossDiscountInstallmentAndReturnDisposition(t *testing.T) {
	f := setupTZFixture(t)
	rootID, settlementID, saleID := uuid.New(), uuid.New(), uuid.New()
	if err := f.db.Exec(`INSERT INTO payments
		(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,
		 concept,balance_pending,payment_date,operator_id,discount_amount)
		VALUES(?,?,1,NOW(),NOW(),'CENT-ROOT',0.02,0.02,'cash','product',0,'2026-08-20',?,0.01)`,
		rootID, f.gymID, f.userID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec(`INSERT INTO payments
		(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,payment_method,
		 concept,balance_pending,parent_payment_id,payment_date,operator_id)
		VALUES(?,?,1,NOW()+INTERVAL '1 second',NOW()+INTERVAL '1 second','CENT-SETTLE',0.03,0.03,
		 'transfer','balance_settlement',0,?,'2026-08-21',?)`,
		settlementID, f.gymID, rootID, f.userID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec(`INSERT INTO sales
		(id,gym_id,version,created_at,updated_at,payment_id,subtotal,discount,total)
		VALUES(?,?,1,NOW(),NOW(),?,0.06,0.01,0.05)`, saleID, f.gymID, rootID).Error; err != nil {
		t.Fatal(err)
	}

	lineIDs := make([]uuid.UUID, 3)
	for i, name := range []string{"Cent A", "Cent B", "Cent C"} {
		productID := uuid.New()
		lineIDs[i] = uuid.New()
		if err := f.db.Exec(`INSERT INTO products
			(id,gym_id,version,created_at,updated_at,name,price,stock)
			VALUES(?,?,1,NOW(),NOW(),?,0.02,1)`, productID, f.gymID, name).Error; err != nil {
			t.Fatal(err)
		}
		if err := f.db.Exec(`INSERT INTO sale_items
			(id,gym_id,version,created_at,updated_at,sale_id,product_id,product_name_snapshot,
			 unit_price_snapshot,unit_cost_snapshot,quantity,line_total)
			VALUES(?,?,1,NOW(),NOW(),?,?,?,0.02,0.01,1,0.02)`,
			lineIDs[i], f.gymID, saleID, productID, name).Error; err != nil {
			t.Fatal(err)
		}
	}

	refundID := uuid.New()
	if err := f.db.Exec(`INSERT INTO refunds
		(id,gym_id,version,created_at,updated_at,root_payment_id,sale_id,amount,method,refunded_on,
		 reason,kind,balance_cancelled,legacy_incomplete,idempotency_key,idempotency_fingerprint,created_by)
		VALUES(?,?,1,NOW(),NOW(),?,?,0.03,'cash','2026-08-22','Dos piezas','revenue_refund',0,FALSE,?,?,?)`,
		refundID, f.gymID, rootID, saleID, "cent-refund", "cent-refund-fingerprint", f.userID).Error; err != nil {
		t.Fatal(err)
	}
	for i, item := range []struct {
		amount      string
		disposition string
	}{{"0.02", "returned_to_stock"}, {"0.01", "damaged"}} {
		if err := f.db.Exec(`INSERT INTO refund_items
			(id,gym_id,version,created_at,updated_at,refund_id,sale_item_id,quantity,amount,disposition)
			VALUES(?,?,1,NOW(),NOW(),?,?,1,?::numeric,?)`,
			uuid.New(), f.gymID, refundID, lineIDs[i], item.amount, item.disposition).Error; err != nil {
			t.Fatal(err)
		}
	}

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reader := reportsInfra.NewPostgresReader()
	read := func(from, to string) (revenue, cogs float64, quantity int) {
		t.Helper()
		rows, readErr := reader.ProductProfitabilityBetween(tx, f.gymID, day(t, from), day(t, to))
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
	canonical, err := reader.CanonicalFinancialBetween(
		tx, f.gymID, cdmxTZ, day(t, "2026-08-20"), day(t, "2026-08-22"),
	)
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
