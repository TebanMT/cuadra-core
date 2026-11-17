//go:build sidecar

package app_test

// UC-048 — ChangeMembershipPlan: orquestación completa contra SQLite real
// (mismas migraciones que producción). Reusa el fixture de
// billing_integration_test.go: socio con "Mensual" $500/30d ACTIVA que
// empezó hoy.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memRepoLite "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/repositories"
)

func (f *billingFixture) changePlan() *billingApp.ChangeMembershipPlan {
	return billingApp.NewChangeMembershipPlan(
		f.paymentRepo, f.folios, f.memberSvc, f.memberRepo, f.uow, f.recorder, billingApp.NoopPublisher{},
	)
}

// newPlan da de alta otro tipo de membresía en el gym del fixture.
func (f *billingFixture) newPlan(name string, price float64, enrollmentFee float64) uuid.UUID {
	f.t.Helper()
	createMT := memApp.NewCreateMembershipType(memRepoLite.NewMembershipTypeSQLiteRepository(), f.uow, f.recorder)
	mt, err := createMT.Execute(context.Background(), memApp.CreateMembershipTypeInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Name: name, Price: price, DurationDays: 30,
		EnrollmentFee: enrollmentFee,
	})
	if err != nil {
		f.t.Fatalf("create plan %s: %v", name, err)
	}
	return mt.ID
}

type membershipRow struct {
	ID         string  `db:"id"`
	Status     string  `db:"status"`
	StartDate  string  `db:"start_date"`
	ExpiryDate *string `db:"expiry_date"`
	ReplacedBy *string `db:"replaced_by"`
	TypeName   string  `db:"type_name_snapshot"`
	PriceCents int64   `db:"price_snapshot"` // SQLite guarda centavos
}

func (f *billingFixture) membershipRows() []membershipRow {
	f.t.Helper()
	var rows []membershipRow
	if err := f.db.Select(&rows, `
		SELECT id, status, start_date, expiry_date, replaced_by,
		       type_name_snapshot, price_snapshot
		FROM memberships WHERE member_id=? ORDER BY created_at, id`,
		f.memberID.String()); err != nil {
		f.t.Fatalf("select memberships: %v", err)
	}
	return rows
}

func TestUC048_HappyPath_UpgradeChargesDeltaAndPreservesExpiry(t *testing.T) {
	f := setup(t)
	// Enrollment fee en el plan nuevo a propósito: UC-048 NUNCA la cobra
	// (DA-48.5 — se liquida al renovar).
	comboID := f.newPlan("Gym + Clases", 700, 150)

	before := f.membershipRows()
	if len(before) != 1 || before[0].Status != "active" {
		t.Fatalf("fixture inesperado: %+v", before)
	}
	oldID, oldExpiry := before[0].ID, *before[0].ExpiryDate

	// Membresía recién activada hoy: restante == ciclo → proporcional = delta.
	uc := f.changePlan()
	out, err := uc.Execute(context.Background(), billingApp.ChangeMembershipPlanInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		NewMembershipTypeID: comboID,
		Amount:              200, // delta = 700 − 500
		Method:              "cash",
	})
	if err != nil {
		t.Fatalf("plan change: %v", err)
	}
	if out.Delta != 200 || out.Prorated != 200 || out.Subtotal != 200 || out.Total != 200 || out.Paid != 200 {
		t.Errorf("montos: delta=%v prorated=%v subtotal=%v total=%v paid=%v (want 200)",
			out.Delta, out.Prorated, out.Subtotal, out.Total, out.Paid)
	}
	if out.ChargeOption != "completa" {
		t.Errorf("charge option = %q, want completa (monto == delta)", out.ChargeOption)
	}
	if out.PaymentID == nil || out.Folio == "" || out.Folio[:4] != "MEM-" {
		t.Errorf("payment/folio: %v %q", out.PaymentID, out.Folio)
	}
	if out.OldTypeName != "Mensual" || out.NewTypeName != "Gym + Clases" {
		t.Errorf("tipos: %q → %q", out.OldTypeName, out.NewTypeName)
	}

	// Memberships: la anterior → replaced apuntando a la nueva; la nueva
	// ACTIVA con el snapshot del combo, inicia hoy y CONSERVA el vencimiento.
	after := f.membershipRows()
	if len(after) != 2 {
		t.Fatalf("memberships = %d, want 2: %+v", len(after), after)
	}
	var old, next *membershipRow
	for i := range after {
		if after[i].ID == oldID {
			old = &after[i]
		} else {
			next = &after[i]
		}
	}
	if old == nil || next == nil {
		t.Fatalf("no se hallaron ambas filas: %+v", after)
	}
	if old.Status != "replaced" || old.ReplacedBy == nil || *old.ReplacedBy != next.ID {
		t.Errorf("anterior: status=%s replaced_by=%v, want replaced → %s", old.Status, old.ReplacedBy, next.ID)
	}
	if next.Status != "active" || next.ID != out.NewMembershipID.String() {
		t.Errorf("nueva: %+v", next)
	}
	if next.ExpiryDate == nil || *next.ExpiryDate != oldExpiry {
		t.Errorf("expiry = %v, want CONSERVADA %q (DA-48.2)", next.ExpiryDate, oldExpiry)
	}
	if next.TypeName != "Gym + Clases" || next.PriceCents != 70000 {
		t.Errorf("snapshot nuevo: %s %d centavos", next.TypeName, next.PriceCents)
	}

	// Payment: exactamente uno, por el delta (sin enrollment — DA-48.5).
	var nPay int
	if err := f.db.Get(&nPay, "SELECT COUNT(*) FROM payments WHERE gym_id=? AND concept='membership' AND amount=20000", f.gymID.String()); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if nPay != 1 {
		t.Errorf("payments de $200 (20000 centavos) = %d, want 1", nPay)
	}
	var enrolled int
	if err := f.db.Get(&enrolled, "SELECT enrollment_paid FROM members WHERE id=?", f.memberID.String()); err != nil {
		t.Fatalf("enrollment_paid: %v", err)
	}
	if enrolled != 0 {
		t.Errorf("UC-048 no debe marcar enrollment pagado")
	}

	// Audit: 1 row plan_change sobre la membresía nueva.
	var nAudit int
	if err := f.db.Get(&nAudit, "SELECT COUNT(*) FROM audit_log WHERE entity_type='memberships' AND action='plan_change'"); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if nAudit != 1 {
		t.Errorf("audit plan_change = %d, want 1", nAudit)
	}
}

func TestUC048_MidCycle_ProratedRounding(t *testing.T) {
	f := setup(t)
	comboID := f.newPlan("Gym + Clases", 700, 0)
	// Reposicionamos la vigencia: empezó hace 16 días, quedan 14 de 30.
	if _, err := f.db.Exec(`UPDATE memberships
		SET start_date=date('now','-16 day'), expiry_date=date('now','+14 day')
		WHERE member_id=? AND status='active'`, f.memberID.String()); err != nil {
		t.Fatalf("reposicionar vigencia: %v", err)
	}

	out, err := f.changePlan().Execute(context.Background(), billingApp.ChangeMembershipPlanInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		NewMembershipTypeID: comboID,
		Amount:              93, // round(200 × 14/30) = round(93.33)
		Method:              "cash",
	})
	if err != nil {
		t.Fatalf("plan change: %v", err)
	}
	if out.CycleDays != 30 || out.DaysRemaining != 14 {
		t.Errorf("días: ciclo=%d restantes=%d, want 30/14", out.CycleDays, out.DaysRemaining)
	}
	if out.Prorated != 93 {
		t.Errorf("prorated = %v, want 93 (redondeo a peso entero)", out.Prorated)
	}
	if out.ChargeOption != "proporcional" {
		t.Errorf("charge option = %q, want proporcional", out.ChargeOption)
	}
	if out.Paid != 93 || out.BalancePending != 0 {
		t.Errorf("paid=%v balance=%v", out.Paid, out.BalancePending)
	}
}

func TestUC048_AmountAboveDelta_RejectsAndRollsBack(t *testing.T) {
	f := setup(t)
	comboID := f.newPlan("Gym + Clases", 700, 0)

	_, err := f.changePlan().Execute(context.Background(), billingApp.ChangeMembershipPlanInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		NewMembershipTypeID: comboID,
		Amount:              250, // delta es 200: el cobro nunca supera la diferencia
		Method:              "cash",
	})
	if !errors.Is(err, billingErrors.ErrPlanChangeAmountExceedsDelta) {
		t.Fatalf("err = %v, want ErrPlanChangeAmountExceedsDelta", err)
	}

	// Rollback total: la membresía original sigue activa, sin filas nuevas
	// ni payments.
	rows := f.membershipRows()
	if len(rows) != 1 || rows[0].Status != "active" || rows[0].TypeName != "Mensual" {
		t.Errorf("el cambio debió revertirse completo: %+v", rows)
	}
	var nPay int
	if err := f.db.Get(&nPay, "SELECT COUNT(*) FROM payments"); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if nPay != 0 {
		t.Errorf("payments = %d, want 0", nPay)
	}
}

func TestUC048_LateralZero_NoPaymentCreated(t *testing.T) {
	f := setup(t)
	lateralID := f.newPlan("Mensual vespertino", 500, 0)

	// Cambio lateral $0: sin método de pago — no hay nada que cobrar.
	out, err := f.changePlan().Execute(context.Background(), billingApp.ChangeMembershipPlanInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		NewMembershipTypeID: lateralID,
		Amount:              0,
	})
	if err != nil {
		t.Fatalf("cambio lateral: %v", err)
	}
	if out.PaymentID != nil || out.Folio != "" {
		t.Errorf("cambio $0 no debe crear Payment (DA-48.6): %v %q", out.PaymentID, out.Folio)
	}
	if out.Delta != 0 || out.Total != 0 || out.ChargeOption != "lateral" {
		t.Errorf("delta=%v total=%v option=%q", out.Delta, out.Total, out.ChargeOption)
	}

	var nPay int
	if err := f.db.Get(&nPay, "SELECT COUNT(*) FROM payments"); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if nPay != 0 {
		t.Errorf("payments = %d, want 0 (nada entra a caja)", nPay)
	}
	// El rastro queda en audit_log + histórico de memberships.
	var nAudit int
	if err := f.db.Get(&nAudit, "SELECT COUNT(*) FROM audit_log WHERE entity_type='memberships' AND action='plan_change'"); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if nAudit != 1 {
		t.Errorf("audit plan_change = %d, want 1", nAudit)
	}
	rows := f.membershipRows()
	if len(rows) != 2 {
		t.Fatalf("memberships = %d, want 2", len(rows))
	}
	var active *membershipRow
	for i := range rows {
		if rows[i].Status == "active" {
			active = &rows[i]
		}
	}
	if active == nil || active.TypeName != "Mensual vespertino" {
		t.Errorf("membresía activa tras el lateral: %+v", active)
	}
}

func TestUC048_Downgrade_Rejected(t *testing.T) {
	f := setup(t)
	cheaperID := f.newPlan("Básico", 350, 0)

	_, err := f.changePlan().Execute(context.Background(), billingApp.ChangeMembershipPlanInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		NewMembershipTypeID: cheaperID,
		Amount:              0,
	})
	if !errors.Is(err, memErrors.ErrPlanChangeDowngrade) {
		t.Fatalf("err = %v, want ErrPlanChangeDowngrade", err)
	}
}

func TestUC048_Expired_Rejected(t *testing.T) {
	f := setup(t)
	comboID := f.newPlan("Gym + Clases", 700, 0)
	if _, err := f.db.Exec(`UPDATE memberships
		SET start_date=date('now','-40 day'), expiry_date=date('now','-10 day')
		WHERE member_id=? AND status='active'`, f.memberID.String()); err != nil {
		t.Fatalf("vencer membresía: %v", err)
	}

	_, err := f.changePlan().Execute(context.Background(), billingApp.ChangeMembershipPlanInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		NewMembershipTypeID: comboID,
		Amount:              200,
		Method:              "cash",
	})
	if !errors.Is(err, memErrors.ErrPlanChangeExpired) {
		t.Fatalf("err = %v, want ErrPlanChangeExpired (vencida → renovación UC-018)", err)
	}
}

func TestUC048_DiscountAndPartial_SameMechanicsAsUC018(t *testing.T) {
	f := setup(t)
	comboID := f.newPlan("Gym + Clases", 700, 0)

	// Descuento sin razón → rechazado (DA-18.2).
	_, err := f.changePlan().Execute(context.Background(), billingApp.ChangeMembershipPlanInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		NewMembershipTypeID: comboID,
		Amount:              200, Discount: 50,
		Method: "cash",
	})
	if !errors.Is(err, billingErrors.ErrDiscountReasonRequired) {
		t.Fatalf("err = %v, want ErrDiscountReasonRequired", err)
	}

	// Descuento con razón + pago parcial → balance pendiente.
	reason := "promo aniversario"
	out, err := f.changePlan().Execute(context.Background(), billingApp.ChangeMembershipPlanInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		NewMembershipTypeID: comboID,
		Amount:              200,
		Discount:            50,
		DiscountReason:      &reason,
		PaidNow:             100,
		Method:              "cash",
	})
	if err != nil {
		t.Fatalf("plan change con descuento/parcial: %v", err)
	}
	if out.Total != 150 || out.Paid != 100 || out.BalancePending != 50 {
		t.Errorf("total=%v paid=%v balance=%v, want 150/100/50", out.Total, out.Paid, out.BalancePending)
	}
	var balanceCents int64
	if err := f.db.Get(&balanceCents, "SELECT balance_pending FROM payments WHERE id=?", out.PaymentID.String()); err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balanceCents != 5000 {
		t.Errorf("balance_pending persistido = %d centavos, want 5000", balanceCents)
	}
}

func TestUC048_IdempotentRetry_ReplaysResult(t *testing.T) {
	f := setup(t)
	comboID := f.newPlan("Gym + Clases", 700, 0)

	in := billingApp.ChangeMembershipPlanInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		NewMembershipTypeID: comboID,
		Amount:              200,
		Method:              "cash",
		IdempotencyKey:      "plan-change-retry-1",
	}
	uc := f.changePlan()
	first, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("primer intento: %v", err)
	}
	second, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("retry debió reproducir el resultado: %v", err)
	}
	if second.PaymentID == nil || first.PaymentID == nil || *second.PaymentID != *first.PaymentID {
		t.Errorf("payment id: %v vs %v", first.PaymentID, second.PaymentID)
	}
	var nPay int
	if err := f.db.Get(&nPay, "SELECT COUNT(*) FROM payments"); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if nPay != 1 {
		t.Errorf("payments = %d, want 1 (retry sin efectos nuevos)", nPay)
	}
	var nMS int
	if err := f.db.Get(&nMS, "SELECT COUNT(*) FROM memberships WHERE member_id=?", f.memberID.String()); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if nMS != 2 {
		t.Errorf("memberships = %d, want 2 (el retry no duplica el cambio)", nMS)
	}
}
