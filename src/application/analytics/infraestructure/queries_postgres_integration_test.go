//go:build server && integration

package infraestructure_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	analyticsInfra "github.com/cuadra/cuadra-core/src/application/analytics/infraestructure"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

type analyticsFixture struct {
	db                                  *gorm.DB
	uow                                 sharedDomain.UnitOfWork
	gymID, userID, monthlyID, premiumID uuid.UUID
}

func setupAnalyticsFixture(t *testing.T) *analyticsFixture {
	t.Helper()
	f := &analyticsFixture{
		db: testutil.OpenPostgres(t), gymID: uuid.New(), userID: uuid.New(),
		monthlyID: uuid.New(), premiumID: uuid.New(),
	}
	f.uow = sharedDomain.NewPostgresUnitOfWork(f.db)
	if err := f.db.Exec(`INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name)
		VALUES (?, ?, 1, NOW(), NOW(), 'Analytics Test')`, f.gymID, f.gymID).Error; err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	if err := f.db.Exec(`INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 'x', 'Owner', 'owner')`,
		f.userID, f.gymID, "analytics-"+f.userID.String()[:8]+"@t.mx").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	for _, p := range []struct {
		id    uuid.UUID
		name  string
		price float64
	}{
		{f.monthlyID, "Mensual", 500}, {f.premiumID, "Premium", 800},
	} {
		if err := f.db.Exec(`INSERT INTO membership_types
			(id, gym_id, version, created_at, updated_at, name, price, duration_days, active)
			VALUES (?, ?, 1, NOW(), NOW(), ?, ?, 30, TRUE)`, p.id, f.gymID, p.name, p.price).Error; err != nil {
			t.Fatalf("seed type: %v", err)
		}
	}
	return f
}

func (f *analyticsFixture) member(t *testing.T, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := f.db.Exec(`INSERT INTO members
		(id, gym_id, version, created_at, updated_at, folio, full_name, phone, created_by)
		VALUES (?, ?, 1, NOW(), NOW(), ?, ?, ?, ?)`, id, f.gymID,
		"SOC-"+id.String()[:8], name, "55"+id.String()[0:8], f.userID).Error; err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return id
}

func (f *analyticsFixture) membership(t *testing.T, id, memberID, typeID uuid.UUID, name string,
	price float64, start, expiry, status string, replacedBy *uuid.UUID) {
	t.Helper()
	if err := f.db.Exec(`INSERT INTO memberships
		(id, gym_id, version, created_at, updated_at, member_id, membership_type_id,
		 type_name_snapshot, price_snapshot, duration_days_snapshot, start_date, expiry_date, status, replaced_by)
		VALUES (?, ?, 1, NOW(), NOW(), ?, ?, ?, ?, 30, ?, ?, ?, ?)`,
		id, f.gymID, memberID, typeID, name, price, start, expiry, status, replacedBy).Error; err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

func TestPG_RenewalHistoryIncludesReplacedAndProjectionUsesOnlyNextMonthDue(t *testing.T) {
	f := setupAnalyticsFixture(t)
	today := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)

	// Renovación normal: la membresía anterior queda status=replaced y con
	// replaced_by. Ese era justo el caso que la query vieja excluía.
	renewedMember := f.member(t, "Renovó")
	oldID, newID := uuid.New(), uuid.New()
	f.membership(t, newID, renewedMember, f.monthlyID, "Mensual", 500,
		"2026-08-01", "2026-09-01", "active", nil)
	f.membership(t, oldID, renewedMember, f.monthlyID, "Mensual", 500,
		"2026-07-01", "2026-08-01", "replaced", &newID)

	notRenewed := f.member(t, "No renovó")
	f.membership(t, uuid.New(), notRenewed, f.monthlyID, "Mensual", 500,
		"2026-06-25", "2026-07-25", "active", nil)

	premiumDue := f.member(t, "Premium por vencer")
	// El snapshot viejo era $700, pero renovar hoy usa el precio de lista
	// vigente ($800) del membership type.
	f.membership(t, uuid.New(), premiumDue, f.premiumID, "Premium anterior", 700,
		"2026-08-20", "2026-09-20", "active", nil)

	// Renovación anticipada: la vigencia que cubre hoy ya está replaced y
	// la nueva active empieza hasta septiembre. El MRR actual no debe perder
	// temporalmente a este socio.
	earlyRenewed := f.member(t, "Renovó anticipadamente")
	earlyOldID, earlyNewID := uuid.New(), uuid.New()
	f.membership(t, earlyNewID, earlyRenewed, f.monthlyID, "Mensual", 500,
		"2026-09-01", "2026-10-01", "active", nil)
	f.membership(t, earlyOldID, earlyRenewed, f.monthlyID, "Mensual", 500,
		"2026-08-01", "2026-09-01", "replaced", &earlyNewID)

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	reader := analyticsInfra.NewPostgresReader()
	current, err := reader.CurrentMonthlyRates(tx, f.gymID, today)
	if err != nil {
		t.Fatalf("current rates: %v", err)
	}
	if len(current) != 2 {
		t.Errorf("current rates = %+v, want two covered members including early renewal", current)
	}
	rates, err := reader.RenewalRatesByType(tx, f.gymID, today)
	if err != nil {
		t.Fatalf("renewal rates: %v", err)
	}
	if len(rates) != 1 || rates[0].TypeName != "Mensual" || rates[0].Expirations != 2 || rates[0].Renewed != 1 {
		t.Errorf("rates = %+v, want Mensual 1/2", rates)
	}

	due, err := reader.RenewalsDueNextMonth(tx, f.gymID, today)
	if err != nil {
		t.Fatalf("due next month: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("due = %+v, want Mensual + Premium", due)
	}
	byType := map[string]float64{}
	for _, r := range due {
		byType[r.TypeName] = r.RenewalAmount
	}
	if byType["Mensual"] != 500 || byType["Premium"] != 800 {
		t.Errorf("renewal amounts = %+v, want full prices 500/800", byType)
	}
}

func TestPG_MonthlyPL_AllocatesCOGSAcrossFiadoAndPartialRefund(t *testing.T) {
	f := setupAnalyticsFixture(t)
	productID, saleID, paymentID, saleItemID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := f.db.Exec(`INSERT INTO products
		(id, gym_id, version, created_at, updated_at, name, price, stock)
		VALUES (?, ?, 1, NOW(), NOW(), 'Accesorio', 10, 10)`, productID, f.gymID).Error; err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if err := f.db.Exec(`INSERT INTO stock_movements
		(id, gym_id, version, created_at, updated_at, product_id, movement_type, delta, cost, is_purchase, operator_id)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 'restock', 10, 5, TRUE, ?)`,
		uuid.New(), f.gymID, productID, f.userID).Error; err != nil {
		t.Fatalf("seed cost: %v", err)
	}
	if err := f.db.Exec(`INSERT INTO payments
		(id, gym_id, version, created_at, updated_at, folio, amount, recognized_amount, payment_method, concept,
		 balance_pending, payment_date, operator_id)
		VALUES (?, ?, 1, NOW(), NOW(), 'PRD-CASH', 40, 40, 'cash', 'product', 20, '2026-08-05', ?)`,
		paymentID, f.gymID, f.userID).Error; err != nil {
		t.Fatalf("seed original payment: %v", err)
	}
	if err := f.db.Exec(`INSERT INTO sales
		(id, gym_id, version, created_at, updated_at, payment_id, subtotal, discount, total)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 60, 0, 60)`, saleID, f.gymID, paymentID).Error; err != nil {
		t.Fatalf("seed sale: %v", err)
	}
	if err := f.db.Exec(`INSERT INTO sale_items
		(id, gym_id, version, created_at, updated_at, sale_id, product_id,
		 product_name_snapshot, unit_price_snapshot, unit_cost_snapshot, quantity, line_total)
		VALUES (?, ?, 1, NOW(), NOW(), ?, ?, 'Accesorio', 10, 5, 6, 60)`,
		saleItemID, f.gymID, saleID, productID).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	settlementID := uuid.New()
	if err := f.db.Exec(`INSERT INTO payments
		(id, gym_id, version, created_at, updated_at, folio, amount, recognized_amount, payment_method, concept,
		 parent_payment_id, balance_pending, payment_date, operator_id)
		VALUES (?, ?, 1, NOW(), NOW(), 'SET-CASH', 20, 20, 'cash', 'balance_settlement', ?, 0, '2026-08-08', ?)`,
		settlementID, f.gymID, paymentID, f.userID).Error; err != nil {
		t.Fatalf("seed settlement: %v", err)
	}
	refundPaymentID := uuid.New()
	if err := f.db.Exec(`INSERT INTO payments
		(id, gym_id, version, created_at, updated_at, folio, amount, recognized_amount, payment_method, concept,
		 parent_payment_id, balance_pending, payment_date, operator_id)
		VALUES (?, ?, 1, NOW(), NOW(), 'REF-CASH', -10, 0, 'cash', 'refund', ?, 0, '2026-08-09', ?)`,
		refundPaymentID, f.gymID, paymentID, f.userID).Error; err != nil {
		t.Fatalf("seed refund payment: %v", err)
	}
	refundID := uuid.New()
	if err := f.db.Exec(`INSERT INTO refunds
		(id,gym_id,version,created_at,updated_at,root_payment_id,refund_payment_id,sale_id,
		 amount,method,refunded_on,reason,kind,balance_cancelled,legacy_incomplete,
		 idempotency_key,idempotency_fingerprint,created_by)
		VALUES(?,?,1,NOW(),NOW(),?,?,?,10,'cash','2026-08-09','parcial','revenue_refund',0,FALSE,?,?,?)`,
		refundID, f.gymID, paymentID, refundPaymentID, saleID,
		"analytics-partial-refund", "analytics-partial-refund-fingerprint", f.userID).Error; err != nil {
		t.Fatalf("seed refund aggregate: %v", err)
	}
	if err := f.db.Exec(`INSERT INTO refund_items
		(id,gym_id,version,created_at,updated_at,refund_id,sale_item_id,quantity,amount,disposition)
		VALUES(?,?,1,NOW(),NOW(),?,?,1,10,'returned_to_stock')`,
		uuid.New(), f.gymID, refundID, saleItemID).Error; err != nil {
		t.Fatalf("seed refund item: %v", err)
	}

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := analyticsInfra.NewPostgresReader().MonthlyPL(tx, f.gymID, 1,
		time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("monthly PL: %v", err)
	}
	if len(rows) != 1 || rows[0].Income != 60 || rows[0].Refunds != 10 || rows[0].COGS != 25 {
		t.Errorf("PL = %+v, want income=60 refunds=10 cogs=25", rows)
	}
}
