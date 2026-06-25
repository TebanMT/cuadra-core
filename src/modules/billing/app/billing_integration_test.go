//go:build sidecar

package app_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	billingRepoLite "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memRepoLite "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/repositories"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

type billingFixture struct {
	t        *testing.T
	db       *sqlx.DB
	uow      sharedDomain.UnitOfWork
	recorder audit.Recorder
	gymID    uuid.UUID
	ownerID  uuid.UUID
	memberID uuid.UUID
	planID   uuid.UUID

	paymentRepo *billingRepoLite.PaymentSQLiteRepository
	memberRepo  *memRepoLite.MemberSQLiteRepository
	gymRepo     *gymRepoLite.GymSQLiteRepository
	memberSvc   *memApp.MemberService
	folios      *folioSvc.Generator
}

func setup(t *testing.T) *billingFixture {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	dsn := dbPath + "?_foreign_keys=on"
	db, err := sqlx.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)

	// Todas las migraciones en orden — no un subset cherry-picked. Así el
	// schema del test matchea producción y no se rompe cada vez que una
	// migración agrega una columna a una tabla base. os.ReadDir ordena por
	// nombre y los archivos están zero-padded (001_..) → orden correcto.
	migDir := "../../../../db_migrations/sqlite"
	migEntries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range migEntries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		m := filepath.Join(migDir, e.Name())
		schema, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(schema)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}
	uow := sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue())
	recorder := audit.NewSQLiteRecorder()

	signup := usersApp.NewSignupOwner(
		usersRepoLite.NewUserSQLiteRepository(),
		gymRepoLite.NewGymSQLiteRepository(),
		uow,
		auth.NewJWTService("test-secret"),
		recorder,
		30,
	)
	owner, err := signup.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName:        "Owner",
		Email:           "owner@gym.com",
		Password:        "supersecret123",
		PasswordConfirm: "supersecret123",
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}

	mtRepo := memRepoLite.NewMembershipTypeSQLiteRepository()
	memberRepo := memRepoLite.NewMemberSQLiteRepository()
	membershipRepo := memRepoLite.NewMembershipSQLiteRepository()
	gymRepo := gymRepoLite.NewGymSQLiteRepository()
	paymentRepo := billingRepoLite.NewPaymentSQLiteRepository()

	memberSvc := memApp.NewMemberService(memberRepo, membershipRepo, mtRepo)

	createMT := memApp.NewCreateMembershipType(mtRepo, uow, recorder)
	mt, err := createMT.Execute(context.Background(), memApp.CreateMembershipTypeInput{
		GymID: owner.GymID, ActorUserID: owner.UserID,
		Name: "Mensual", Price: 500, DurationDays: 30,
		EnrollmentFee: 100,
	})
	if err != nil {
		t.Fatalf("create mt: %v", err)
	}

	createMember := memApp.NewCreateMember(memberRepo, membershipRepo, mtRepo, uow, recorder)
	mem, err := createMember.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: owner.GymID, ActorUserID: owner.UserID,
		FullName: "Juan Pérez", Phone: "+524421234567",
		MembershipTypeID: mt.ID, StartDate: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	// Las pruebas de UC-018/UC-019/UC-022 asumen una membresía activa
	// como punto de partida; CreateMember sin pago la deja en
	// pending_payment, así que la activamos vía el seam de members
	// (esto equivale al "primer abono" sin tener que crear un Payment
	// extra que distorsione los tests).
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := memberSvc.RenewMembershipForPayment(context.Background(), tx, memApp.RenewMembershipForPaymentInput{
			MemberID: mem.MemberID, MembershipTypeID: mt.ID, PaymentDate: time.Now().UTC(),
		}, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatalf("activate membership: %v", err)
	}

	return &billingFixture{
		t:           t,
		db:          db,
		uow:         uow,
		recorder:    recorder,
		gymID:       owner.GymID,
		ownerID:     owner.UserID,
		memberID:    mem.MemberID,
		planID:      mt.ID,
		paymentRepo: paymentRepo,
		memberRepo:  memberRepo,
		gymRepo:     gymRepo,
		memberSvc:   memberSvc,
		folios:      folioSvc.NewGenerator(paymentRepo),
	}
}

func (f *billingFixture) registerPayment() *billingApp.RegisterMembershipPayment {
	return billingApp.NewRegisterMembershipPayment(
		f.paymentRepo, f.folios, f.memberSvc, f.memberRepo, f.uow, f.recorder, billingApp.NoopPublisher{},
	)
}

// ---------------------------------------------------------------------------
// UC-018 — RegisterMembershipPayment
// ---------------------------------------------------------------------------

func TestUC018_HappyPath_FirstPaymentChargesEnrollmentAndRenews(t *testing.T) {
	f := setup(t)
	uc := f.registerPayment()
	out, err := uc.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "cash",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if out.Subtotal != 600 || out.Total != 600 || out.Paid != 600 {
		t.Errorf("amounts = subtotal=%v total=%v paid=%v (expected 600 each)", out.Subtotal, out.Total, out.Paid)
	}
	if !out.EnrollmentChrg {
		t.Errorf("first payment should charge enrollment")
	}
	if out.Folio == "" || out.Folio[:4] != "MEM-" {
		t.Errorf("folio = %q", out.Folio)
	}

	// Atomicity: payment + new membership + audit must all be present.
	var nPay, nMS, nAudit int
	if err := f.db.Get(&nPay, "SELECT COUNT(*) FROM payments WHERE gym_id=?", f.gymID.String()); err != nil {
		t.Fatalf("count pay: %v", err)
	}
	if err := f.db.Get(&nMS, "SELECT COUNT(*) FROM memberships WHERE member_id=? AND status='active'", f.memberID.String()); err != nil {
		t.Fatalf("count ms: %v", err)
	}
	if err := f.db.Get(&nAudit, "SELECT COUNT(*) FROM audit_log WHERE entity_type='payments'"); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if nPay != 1 {
		t.Errorf("payments count = %d, want 1", nPay)
	}
	if nMS != 1 {
		t.Errorf("active memberships = %d, want 1 (renewal replaces previous)", nMS)
	}
	if nAudit != 1 {
		t.Errorf("audit rows = %d, want 1", nAudit)
	}

	// Member now has enrollment_paid = true.
	var enrolled int
	if err := f.db.Get(&enrolled, "SELECT enrollment_paid FROM members WHERE id=?", f.memberID.String()); err != nil {
		t.Fatalf("get enrollment_paid: %v", err)
	}
	if enrolled != 1 {
		t.Errorf("enrollment_paid not flipped")
	}

	// A second payment should NOT recharge enrollment.
	out2, err := uc.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "transfer",
	})
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	if out2.EnrollmentChrg {
		t.Errorf("enrollment should not be recharged")
	}
	if out2.Subtotal != 500 {
		t.Errorf("second subtotal = %v, want 500", out2.Subtotal)
	}
	if out2.Folio == out.Folio {
		t.Errorf("folios collide: %s == %s", out.Folio, out2.Folio)
	}
}

// Socio huérfano (sin membresía vigente — p.ej. importado con la suya ya
// vencida, o tras una cancelación): cobrar debe RE-INSCRIBIRLO creando y
// activando una membresía nueva, en vez de fallar con "el socio no tiene
// membresía vigente".
func TestUC018_OrphanMember_PaymentReenrolls(t *testing.T) {
	f := setup(t)
	// Dejamos su única membresía en 'expired' → GetCurrentByMember no halla
	// ninguna active/pending y el socio queda huérfano.
	if _, err := f.db.Exec("UPDATE memberships SET status='expired' WHERE member_id=?", f.memberID.String()); err != nil {
		t.Fatalf("orphan setup: %v", err)
	}

	uc := f.registerPayment()
	out, err := uc.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "cash",
	})
	if err != nil {
		t.Fatalf("cobro de socio huérfano debió re-inscribir, no fallar: %v", err)
	}
	if out.Paid <= 0 {
		t.Errorf("paid = %v, want > 0", out.Paid)
	}

	// Exactamente una membresía activa nueva...
	var nActive int
	if err := f.db.Get(&nActive, "SELECT COUNT(*) FROM memberships WHERE member_id=? AND status='active'", f.memberID.String()); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if nActive != 1 {
		t.Errorf("active memberships = %d, want 1 (re-inscripción)", nActive)
	}
	// ...y la vieja 'expired' sigue como historial (no se borra).
	var nExpired int
	if err := f.db.Get(&nExpired, "SELECT COUNT(*) FROM memberships WHERE member_id=? AND status='expired'", f.memberID.String()); err != nil {
		t.Fatalf("count expired: %v", err)
	}
	if nExpired != 1 {
		t.Errorf("expired memberships = %d, want 1 (historial preservado)", nExpired)
	}
}

// fakeWelcome captura las llamadas al seam de bienvenida.
type fakeWelcome struct {
	calls      int
	lastNumber int
}

func (w *fakeWelcome) Notify(_ context.Context, _ sharedDomain.Transaction, in memApp.WelcomeNotifyInput, _ time.Time) (memApp.WelcomeDispatchResult, error) {
	w.calls++
	w.lastNumber = in.Number
	return memApp.WelcomeDispatchResult{}, nil
}

// El WhatsApp de bienvenida se manda cuando el socio queda ACTIVO por primera
// vez (su primer pago), y NO se repite en renovaciones.
func TestUC018_WelcomeFiresOnFirstActivationOnly(t *testing.T) {
	f := setup(t)
	// Socio con número asignado y su membresía aún sin pagar (pending_payment).
	if _, err := f.db.Exec("UPDATE members SET member_number=4321 WHERE id=?", f.memberID.String()); err != nil {
		t.Fatalf("set number: %v", err)
	}
	if _, err := f.db.Exec("UPDATE memberships SET status='pending_payment', expiry_date=NULL WHERE member_id=?", f.memberID.String()); err != nil {
		t.Fatalf("pending: %v", err)
	}

	welcome := &fakeWelcome{}
	uc := billingApp.NewRegisterMembershipPayment(
		f.paymentRepo, f.folios, f.memberSvc, f.memberRepo, f.uow, f.recorder, billingApp.NoopPublisher{},
	).WithWelcomeNotifier(welcome)
	in := billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash",
	}

	// 1er pago → activa por primera vez → welcome 1 vez, con el número.
	if _, err := uc.Execute(context.Background(), in); err != nil {
		t.Fatalf("primer pago: %v", err)
	}
	if welcome.calls != 1 {
		t.Fatalf("welcome tras 1ra activación = %d, want 1", welcome.calls)
	}
	if welcome.lastNumber != 4321 {
		t.Errorf("welcome number = %d, want 4321", welcome.lastNumber)
	}

	// 2do pago → renovación → NO se re-envía.
	if _, err := uc.Execute(context.Background(), in); err != nil {
		t.Fatalf("segundo pago: %v", err)
	}
	if welcome.calls != 1 {
		t.Errorf("welcome tras renovación = %d, want 1 (no re-envío)", welcome.calls)
	}
}

// El generador de folios debe usar el valor NUMÉRICO del folio, no el orden
// lexicográfico: "MEM-99999" (5 díg) es lexicográficamente MAYOR que
// "MEM-100000" (6 díg) aunque sea numéricamente menor. Sin esto, el siguiente
// folio se recalculaba como MEM-100000 y colisionaba con el ya existente
// (repro del UNIQUE constraint tras importar pagos con MEM-%05d de IDs legacy
// que pasan de 99999).
func TestFolioGenerator_NumericMaxNotLexicographic(t *testing.T) {
	f := setup(t)
	for _, folio := range []string{"MEM-99999", "MEM-100000"} {
		if _, err := f.db.Exec(
			`INSERT INTO payments
			   (id, gym_id, version, created_at, updated_at, folio, member_id,
			    amount, payment_method, concept, balance_pending, payment_date, operator_id)
			 VALUES (?, ?, 1, 1, 1, ?, ?, 10000, 'cash', 'membership', 0, '2026-01-01', ?)`,
			uuid.New().String(), f.gymID.String(), folio, f.memberID.String(), f.ownerID.String(),
		); err != nil {
			t.Fatalf("insert %s: %v", folio, err)
		}
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	next, err := f.folios.Next(tx, f.gymID, "membership")
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if next != "MEM-100001" {
		t.Errorf("siguiente folio = %q, want MEM-100001 (numérico, no MEM-100000 lexicográfico)", next)
	}
}

func TestUC018_PartialPayment_ExtendsAndCarriesBalance(t *testing.T) {
	f := setup(t)
	uc := f.registerPayment()
	out, err := uc.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "cash", PaidNow: 300, // total is 600 (500 plan + 100 enroll)
	})
	if err != nil {
		t.Fatalf("partial: %v", err)
	}
	if out.Paid != 300 || out.BalancePending != 300 {
		t.Errorf("partial result = paid=%v balance=%v", out.Paid, out.BalancePending)
	}
	// New membership exists (DA-18.1 — partial extends membership).
	var nActive int
	if err := f.db.Get(&nActive, "SELECT COUNT(*) FROM memberships WHERE member_id=? AND status='active'", f.memberID.String()); err != nil {
		t.Fatalf("count: %v", err)
	}
	if nActive != 1 {
		t.Errorf("active memberships = %d, expected 1", nActive)
	}
}

func TestUC018_DiscountRequiresReason(t *testing.T) {
	f := setup(t)
	uc := f.registerPayment()
	_, err := uc.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "cash", Discount: 100,
	})
	if err == nil {
		t.Errorf("discount without reason should fail")
	}
	r := "Promo de aniversario"
	if _, err := uc.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "cash", Discount: 100, DiscountReason: &r,
	}); err != nil {
		t.Errorf("discount with reason should pass: %v", err)
	}
}

func TestUC018_RejectsBadInputs(t *testing.T) {
	f := setup(t)
	uc := f.registerPayment()
	if _, err := uc.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
	}); err == nil {
		t.Errorf("missing method should fail")
	}
}

// Property-style: run UC-018 twice with same input — both must succeed
// (no folio collision, no membership invariant violation, both rows committed).
func TestUC018_TwoSequentialRunsAreAtomicAndIdempotentlyOrderable(t *testing.T) {
	f := setup(t)
	uc := f.registerPayment()
	in := billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash",
	}
	a, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a.Folio == b.Folio {
		t.Errorf("folios duplicated: %s", a.Folio)
	}
	var n int
	if err := f.db.Get(&n, "SELECT COUNT(*) FROM payments WHERE gym_id=?", f.gymID.String()); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("payments count = %d, want 2", n)
	}
}

// ---------------------------------------------------------------------------
// UC-019 — SettlePendingBalance
// ---------------------------------------------------------------------------

func TestUC019_SettlementCreatesNewRowAndDecrementsBalance(t *testing.T) {
	f := setup(t)
	register := f.registerPayment()
	out, err := register.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash", PaidNow: 300,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	settle := billingApp.NewSettlePendingBalance(f.paymentRepo, f.folios, f.uow, f.recorder)
	res, err := settle.Execute(context.Background(), billingApp.SettlePendingBalanceInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ParentPaymentID: out.PaymentID,
		Amount:          150, Method: "transfer",
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if res.NewBalancePending != 150 {
		t.Errorf("balance after partial settle = %v, want 150", res.NewBalancePending)
	}
	// Parent payment row should reflect the decremented balance.
	var bal int64
	if err := f.db.Get(&bal, "SELECT balance_pending FROM payments WHERE id=?", out.PaymentID.String()); err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal != 15000 { // cents
		t.Errorf("parent balance_pending = %d, want 15000", bal)
	}
}

func TestUC019_SettlementOverBalanceRejected(t *testing.T) {
	f := setup(t)
	register := f.registerPayment()
	out, _ := register.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash", PaidNow: 500,
	})
	settle := billingApp.NewSettlePendingBalance(f.paymentRepo, f.folios, f.uow, f.recorder)
	if _, err := settle.Execute(context.Background(), billingApp.SettlePendingBalanceInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ParentPaymentID: out.PaymentID,
		Amount:          200, Method: "cash",
	}); err == nil {
		t.Errorf("over balance should fail")
	}
}

// ---------------------------------------------------------------------------
// UC-020 — GenerateReceipt (PDF in-memory)
// ---------------------------------------------------------------------------

func TestUC020_GenerateReceipt_ReturnsPDFBytes(t *testing.T) {
	f := setup(t)
	register := f.registerPayment()
	out, err := register.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	uc := billingApp.NewGenerateReceipt(f.paymentRepo, f.gymRepo, f.memberRepo, f.uow)
	pdf, err := uc.Execute(context.Background(), billingApp.GenerateReceiptInput{
		GymID: f.gymID, PaymentID: out.PaymentID,
	})
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	if pdf.ContentType != "application/pdf" {
		t.Errorf("content-type = %q", pdf.ContentType)
	}
	if !bytes.HasPrefix(pdf.PDF, []byte("%PDF-")) {
		t.Errorf("output not a PDF, head=%q", pdf.PDF[:min(8, len(pdf.PDF))])
	}
	if !bytes.Contains(pdf.PDF, []byte("%%EOF")) {
		t.Errorf("PDF missing trailer")
	}
}

// ---------------------------------------------------------------------------
// UC-021 — ListMemberPayments
// ---------------------------------------------------------------------------

func TestUC021_ListMemberPayments_FiltersAndPaginates(t *testing.T) {
	f := setup(t)
	register := f.registerPayment()
	for i := 0; i < 3; i++ {
		_, err := register.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
			GymID: f.gymID, ActorUserID: f.ownerID,
			MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash",
		})
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	uc := billingApp.NewListMemberPayments(f.paymentRepo, f.memberRepo, f.uow)
	out, err := uc.Execute(context.Background(), billingApp.ListMemberPaymentsInput{
		GymID: f.gymID, MemberID: f.memberID,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out.Total != 3 || len(out.Items) != 3 {
		t.Errorf("total=%d items=%d, want 3", out.Total, len(out.Items))
	}
	// Filter by concept = membership returns the same 3.
	out, _ = uc.Execute(context.Background(), billingApp.ListMemberPaymentsInput{
		GymID: f.gymID, MemberID: f.memberID, ConceptFilter: "membership",
	})
	if out.Total != 3 {
		t.Errorf("filtered = %d", out.Total)
	}
	// Pagination cuts.
	out, _ = uc.Execute(context.Background(), billingApp.ListMemberPaymentsInput{
		GymID: f.gymID, MemberID: f.memberID, PageSize: 2, Page: 1,
	})
	if len(out.Items) != 2 || out.Total != 3 {
		t.Errorf("page 1 of 2: items=%d total=%d", len(out.Items), out.Total)
	}
}

// ---------------------------------------------------------------------------
// UC-022 — RefundPayment
// ---------------------------------------------------------------------------

func TestUC022_RefundCreatesNegativeRowAppendOnly(t *testing.T) {
	f := setup(t)
	register := f.registerPayment()
	out, _ := register.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash",
	})
	uc := billingApp.NewRefundPayment(f.paymentRepo, f.folios, f.memberSvc, f.uow, f.recorder)
	res, err := uc.Execute(context.Background(), billingApp.RefundPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ParentPaymentID: out.PaymentID, Reason: "Cliente cambió de opinión",
		Method: "cash",
	})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if res.Amount >= 0 {
		t.Errorf("refund amount should be negative: %v", res.Amount)
	}
	// Original payment row UNCHANGED in amount (append-only).
	var origAmount int64
	if err := f.db.Get(&origAmount, "SELECT amount FROM payments WHERE id=?", out.PaymentID.String()); err != nil {
		t.Fatalf("get orig: %v", err)
	}
	if origAmount != 60000 { // cents
		t.Errorf("original amount mutated: %d", origAmount)
	}
	// Second refund attempt fails.
	if _, err := uc.Execute(context.Background(), billingApp.RefundPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ParentPaymentID: out.PaymentID, Reason: "doble", Method: "cash",
	}); err == nil {
		t.Errorf("double refund should be rejected")
	}
}

func TestUC022_RefundWithRevertCancelsRenewal(t *testing.T) {
	f := setup(t)
	register := f.registerPayment()
	// First payment (initial Membership was created at member creation).
	first, _ := register.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash",
	})
	// Active count = 1 (renewed); previous one is `replaced`.
	var nActive int
	_ = f.db.Get(&nActive, "SELECT COUNT(*) FROM memberships WHERE member_id=? AND status='active'", f.memberID.String())
	if nActive != 1 {
		t.Fatalf("expected 1 active before refund, got %d", nActive)
	}

	uc := billingApp.NewRefundPayment(f.paymentRepo, f.folios, f.memberSvc, f.uow, f.recorder)
	res, err := uc.Execute(context.Background(), billingApp.RefundPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ParentPaymentID: first.PaymentID, Reason: "Error en cobro",
		Method: "cash", RevertMembership: true,
	})
	if err != nil {
		t.Fatalf("refund w/ revert: %v", err)
	}
	if !res.Reverted {
		t.Errorf("expected reverted=true")
	}
	// After revert: previous membership should be active again, current cancelled.
	if err := f.db.Get(&nActive, "SELECT COUNT(*) FROM memberships WHERE member_id=? AND status='active'", f.memberID.String()); err != nil {
		t.Fatalf("count: %v", err)
	}
	if nActive != 1 {
		t.Errorf("expected 1 active after revert (predecessor restored), got %d", nActive)
	}
	var nCancelled int
	if err := f.db.Get(&nCancelled, "SELECT COUNT(*) FROM memberships WHERE member_id=? AND status='cancelled'", f.memberID.String()); err != nil {
		t.Fatalf("count cancelled: %v", err)
	}
	if nCancelled != 1 {
		t.Errorf("expected 1 cancelled (the renewal), got %d", nCancelled)
	}
}

func TestUC022_RefundReasonRequired(t *testing.T) {
	f := setup(t)
	register := f.registerPayment()
	out, _ := register.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash",
	})
	uc := billingApp.NewRefundPayment(f.paymentRepo, f.folios, f.memberSvc, f.uow, f.recorder)
	if _, err := uc.Execute(context.Background(), billingApp.RefundPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ParentPaymentID: out.PaymentID, Reason: "  ", Method: "cash",
	}); err == nil {
		t.Errorf("blank reason should fail")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
