//go:build sidecar

package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	billingRepoLite "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	memRepoLite "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/repositories"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// fixture wires a fresh in-memory-ish (file-backed temp) SQLite DB with
// schema applied + an owner+gym signed up. Returns DB handle, UoW, and
// owner/gym IDs.
type membersFixture struct {
	t        *testing.T
	db       *sqlx.DB
	uow      sharedDomain.UnitOfWork
	recorder audit.Recorder
	gymID    uuid.UUID
	ownerID  uuid.UUID

	mtRepo      *memRepoLite.MembershipTypeSQLiteRepository
	memberRepo  *memRepoLite.MemberSQLiteRepository
	membershipR *memRepoLite.MembershipSQLiteRepository
	adjustmentR *memRepoLite.MembershipAdjustmentSQLiteRepository
	paymentRepo *billingRepoLite.PaymentSQLiteRepository
	folios      *folioSvc.Generator
}

func setupMembersFixture(t *testing.T) *membersFixture {
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
	out, err := signup.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName:        "Owner",
		Email:           "owner@gym.com",
		Password:        "supersecret123",
		PasswordConfirm: "supersecret123",
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	paymentRepo := billingRepoLite.NewPaymentSQLiteRepository()
	folios := folioSvc.NewGenerator(paymentRepo)
	return &membersFixture{
		t:           t,
		db:          db,
		uow:         uow,
		recorder:    recorder,
		gymID:       out.GymID,
		ownerID:     out.UserID,
		mtRepo:      memRepoLite.NewMembershipTypeSQLiteRepository(),
		memberRepo:  memRepoLite.NewMemberSQLiteRepository(),
		membershipR: memRepoLite.NewMembershipSQLiteRepository(),
		adjustmentR: memRepoLite.NewMembershipAdjustmentSQLiteRepository(),
		paymentRepo: paymentRepo,
		folios:      folios,
	}
}

func (f *membersFixture) createMembershipType(t *testing.T, name string) uuid.UUID {
	t.Helper()
	uc := memApp.NewCreateMembershipType(f.mtRepo, f.uow, f.recorder)
	out, err := uc.Execute(context.Background(), memApp.CreateMembershipTypeInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Name: name, Price: 500, DurationDays: 30,
	})
	if err != nil {
		t.Fatalf("createMT: %v", err)
	}
	return out.ID
}

// ---------------------------------------------------------------------------
// UC-011 — MembershipType CRUD
// ---------------------------------------------------------------------------

func TestUC011_MembershipTypeCRUD(t *testing.T) {
	f := setupMembersFixture(t)
	createMT := memApp.NewCreateMembershipType(f.mtRepo, f.uow, f.recorder)
	updateMT := memApp.NewUpdateMembershipType(f.mtRepo, f.uow, f.recorder)
	deactivateMT := memApp.NewDeactivateMembershipType(f.mtRepo, f.uow, f.recorder)
	listMT := memApp.NewListMembershipTypes(f.mtRepo, f.uow)

	created, err := createMT.Execute(context.Background(), memApp.CreateMembershipTypeInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Name: "Mensual", Price: 500, DurationDays: 30,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Duplicate name => business error.
	if _, err := createMT.Execute(context.Background(), memApp.CreateMembershipTypeInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Name: "MENSUAL", Price: 500, DurationDays: 30,
	}); err == nil {
		t.Errorf("duplicate name should fail")
	}

	updated, err := updateMT.Execute(context.Background(), memApp.UpdateMembershipTypeInput{
		GymID: f.gymID, ActorUserID: f.ownerID, TypeID: created.ID,
		Name: "Mensual Premium", Price: 600, DurationDays: 30,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Mensual Premium" || updated.Price != 600 {
		t.Errorf("update: %+v", updated)
	}

	if _, err := deactivateMT.Execute(context.Background(), memApp.DeactivateMembershipTypeInput{
		GymID: f.gymID, ActorUserID: f.ownerID, TypeID: created.ID,
	}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	active, err := listMT.Execute(context.Background(), memApp.ListMembershipTypesInput{GymID: f.gymID})
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("active list = %d, want 0 (was deactivated)", len(active))
	}
	all, err := listMT.Execute(context.Background(), memApp.ListMembershipTypesInput{GymID: f.gymID, IncludeInactive: true})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("all list = %d, want 1", len(all))
	}
}

// ---------------------------------------------------------------------------
// UC-012 — Crear socio (incluye Membership atómica + folio)
// ---------------------------------------------------------------------------

func TestUC012_CreateMember_HappyPath(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")

	// Happy path con ChargeFirstPayment=true requiere billing wirado;
	// inline acá para no contaminar la fixture común con deps que el
	// resto de tests no necesita.
	paymentRepo := billingRepoLite.NewPaymentSQLiteRepository()
	folios := folioSvc.NewGenerator(paymentRepo)
	uc := memApp.NewCreateMemberWithBilling(f.memberRepo, f.membershipR, f.mtRepo, paymentRepo, folios, f.uow, f.recorder)
	out, err := uc.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName:           "Juan Pérez García",
		Phone:              "+524421234567",
		MembershipTypeID:   typeID,
		StartDate:          time.Now().UTC(),
		ChargeFirstPayment: true,
		PaymentMethod:      "cash",
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	if out.Folio != "MEM-000001" {
		t.Errorf("folio = %q, want MEM-000001", out.Folio)
	}
	if !out.PendingFirstPayment {
		t.Errorf("PendingFirstPayment should be true")
	}
	if out.PaymentID == nil {
		t.Errorf("PaymentID nil — primer pago no se registró")
	}
	if out.PaymentFolio == "" {
		t.Errorf("PaymentFolio vacío")
	}
	var paymentCount int
	if err := f.db.Get(&paymentCount, "SELECT COUNT(*) FROM payments"); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if paymentCount != 1 {
		t.Errorf("payments count = %d, want 1", paymentCount)
	}

	var n int
	if err := f.db.Get(&n, "SELECT COUNT(*) FROM members"); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("members count = %d", n)
	}
	if err := f.db.Get(&n, "SELECT COUNT(*) FROM memberships WHERE status='active'"); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if n != 1 {
		t.Errorf("active memberships = %d", n)
	}

	// Folio increments on the next member.
	out2, err := uc.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "María López", Phone: "+524429876543",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create 2nd: %v", err)
	}
	if out2.Folio != "MEM-000002" {
		t.Errorf("2nd folio = %q", out2.Folio)
	}
}

func TestUC012_CreateMember_DuplicatePhone(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	uc := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)

	in := memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Juan Pérez", Phone: "+524421234567",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
	}
	if _, err := uc.Execute(context.Background(), in); err != nil {
		t.Fatalf("first: %v", err)
	}
	in.FullName = "Otro Familiar"
	_, err := uc.Execute(context.Background(), in)
	if err == nil {
		t.Errorf("duplicate phone should fail without AllowDuplicatePhone")
	}
	in.AllowDuplicatePhone = true
	if _, err := uc.Execute(context.Background(), in); err != nil {
		t.Errorf("AllowDuplicatePhone should bypass: %v", err)
	}
}

func TestUC012_CreateMember_InactiveTypeRejected(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	deactivateMT := memApp.NewDeactivateMembershipType(f.mtRepo, f.uow, f.recorder)
	if _, err := deactivateMT.Execute(context.Background(), memApp.DeactivateMembershipTypeInput{
		GymID: f.gymID, ActorUserID: f.ownerID, TypeID: typeID,
	}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	uc := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	if _, err := uc.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Juan", Phone: "+524421234567",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
	}); err == nil {
		t.Errorf("inactive plan should be rejected")
	}
}

// ---------------------------------------------------------------------------
// UC-013 — Editar socio
// ---------------------------------------------------------------------------

func TestUC013_UpdateMember(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	create := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	created, err := create.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Juan", Phone: "+524421234567",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	uc := memApp.NewUpdateMember(f.memberRepo, f.uow, f.recorder)
	newName := "Juan Pérez García"
	updated, err := uc.Execute(context.Background(), memApp.UpdateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: created.MemberID,
		FullName: &newName,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.FullName != newName {
		t.Errorf("name = %q", updated.FullName)
	}
}

// ---------------------------------------------------------------------------
// UC-014 — Listar / buscar / filtrar
// ---------------------------------------------------------------------------

func TestUC014_ListMembers_FiltersAndSearch(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	create := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)

	for i, name := range []string{"Ana López", "Bruno Ramos", "Carlos Téllez"} {
		_, err := create.Execute(context.Background(), memApp.CreateMemberInput{
			GymID: f.gymID, ActorUserID: f.ownerID,
			FullName: name, Phone: phoneFor(i),
			MembershipTypeID: typeID, StartDate: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	uc := memApp.NewListMembers(f.memberRepo, f.uow)
	out, err := uc.Execute(context.Background(), memApp.ListMembersInput{GymID: f.gymID, Sort: "name", SortAscending: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out.Total != 3 || len(out.Items) != 3 {
		t.Errorf("total=%d items=%d", out.Total, len(out.Items))
	}
	if out.Items[0].Member.FullName != "Ana López" {
		t.Errorf("sort: first = %s", out.Items[0].Member.FullName)
	}
	// search by name prefix
	out, _ = uc.Execute(context.Background(), memApp.ListMembersInput{GymID: f.gymID, Search: "Bru"})
	if out.Total != 1 || out.Items[0].Member.FullName != "Bruno Ramos" {
		t.Errorf("search Bru: %+v", out.Items)
	}
}

// ---------------------------------------------------------------------------
// UC-015 — Detail
// ---------------------------------------------------------------------------

func TestUC015_MemberDetail_IncludesCurrentMembership(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	create := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	created, err := create.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Juan Pérez", Phone: "+524421234567",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// FingerprintRepository real para que GetMemberDetail pueda
	// resolver has_fingerprint (devuelve false cuando no hay rows, que
	// es justo el estado de este socio recién creado).
	uc := memApp.NewGetMemberDetail(f.memberRepo, memRepoLite.NewFingerprintSQLiteRepository(), f.uow)
	out, err := uc.Execute(context.Background(), memApp.GetMemberDetailInput{GymID: f.gymID, MemberID: created.MemberID})
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if out.CurrentMembership == nil {
		t.Fatalf("expected current membership")
	}
	if out.AccessStatus == "" {
		t.Errorf("expected access status")
	}
}

// ---------------------------------------------------------------------------
// UC-016 — Toggle status
// ---------------------------------------------------------------------------

func TestUC016_ToggleStatus(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	create := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	created, _ := create.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Juan", Phone: "+524421234567",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
	})
	uc := memApp.NewToggleMemberStatus(f.memberRepo, f.uow, f.recorder)
	out, err := uc.Execute(context.Background(), memApp.ToggleMemberStatusInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: created.MemberID,
		NewStatus: "inactive", Reason: "Se mudó",
	})
	if err != nil {
		t.Fatalf("toggle: %v", err)
	}
	if out.Status != "inactive" {
		t.Errorf("status = %q", out.Status)
	}
}

// ---------------------------------------------------------------------------
// UC-017 — Lock expiry: atomicidad UPDATE memberships + INSERT adjustments
// ---------------------------------------------------------------------------

func TestUC017_LockExpiry_AtomicAdjustment(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	// ChargeFirstPayment ⇒ la membresía nace activa (con expiry); el
	// lock sólo aplica sobre memberships activas, no sobre pending.
	create := memApp.NewCreateMemberWithBilling(f.memberRepo, f.membershipR, f.mtRepo, f.paymentRepo, f.folios, f.uow, f.recorder)
	created, err := create.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Juan", Phone: "+524421234567",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
		ChargeFirstPayment: true,
		PaymentMethod:      "cash",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	uc := memApp.NewLockMembershipExpiry(f.membershipR, f.adjustmentR, f.uow, f.recorder)
	out, err := uc.Execute(context.Background(), memApp.LockMembershipExpiryInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MembershipID: created.MembershipID,
		Mode: memApp.ModeExtendDays, Days: 14, Reason: "Cortesía COVID",
	})
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if out.DaysAdded != 14 {
		t.Errorf("days = %d", out.DaysAdded)
	}
	// Both UPDATE and INSERT must have committed.
	var rowExpiry string
	if err := f.db.Get(&rowExpiry, `SELECT expiry_date FROM memberships WHERE id=?`, created.MembershipID.String()); err != nil {
		t.Fatalf("get expiry: %v", err)
	}
	if rowExpiry != out.NewExpiry.Format("2006-01-02") {
		t.Errorf("db expiry = %q, expected %q", rowExpiry, out.NewExpiry.Format("2006-01-02"))
	}
	var nAdj int
	if err := f.db.Get(&nAdj, `SELECT COUNT(*) FROM membership_adjustments WHERE membership_id=?`, created.MembershipID.String()); err != nil {
		t.Fatalf("count adj: %v", err)
	}
	if nAdj != 1 {
		t.Errorf("adj count = %d, want 1", nAdj)
	}

	// Reason too short is rejected — and the membership should NOT have been
	// updated (atomicity).
	beforeExpiry := rowExpiry
	if _, err := uc.Execute(context.Background(), memApp.LockMembershipExpiryInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MembershipID: created.MembershipID,
		Mode: memApp.ModeExtendDays, Days: 5, Reason: "abc",
	}); err == nil {
		t.Errorf("short reason should fail")
	}
	var afterExpiry string
	if err := f.db.Get(&afterExpiry, `SELECT expiry_date FROM memberships WHERE id=?`, created.MembershipID.String()); err != nil {
		t.Fatalf("get expiry after: %v", err)
	}
	if afterExpiry != beforeExpiry {
		t.Errorf("expiry mutated despite reason failure: before=%q after=%q", beforeExpiry, afterExpiry)
	}
}

// ---------------------------------------------------------------------------
// UC-032 (partial) — AssignMemberNumber (ADR-010)
// ---------------------------------------------------------------------------

func TestAssignMemberNumber_GeneratesUnique(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	create := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	out1, _ := create.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Juan", Phone: "+524421234567",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
	})
	out2, _ := create.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Maria", Phone: "+524429876543",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
	})
	uc := memApp.NewAssignMemberNumber(f.memberRepo, f.uow, f.recorder)
	a1, err := uc.Execute(context.Background(), memApp.AssignMemberNumberInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: out1.MemberID, Number: 5839,
	})
	if err != nil {
		t.Fatalf("assign 1: %v", err)
	}
	if a1.MemberNumber != 5839 {
		t.Errorf("number = %d", a1.MemberNumber)
	}
	// Second member tries to take the SAME number -> should fail.
	if _, err := uc.Execute(context.Background(), memApp.AssignMemberNumberInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: out2.MemberID, Number: 5839,
	}); err == nil {
		t.Errorf("duplicate number should fail")
	}
	// Auto-generated number should succeed and stay in the 4-digit range.
	a2, err := uc.Execute(context.Background(), memApp.AssignMemberNumberInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: out2.MemberID,
	})
	if err != nil {
		t.Fatalf("auto-gen: %v", err)
	}
	if a2.MemberNumber == 5839 {
		t.Errorf("auto-gen collided")
	}
	if a2.MemberNumber < 1000 || a2.MemberNumber > 9999 {
		t.Errorf("auto number fuera del rango de 4 dígitos: %d", a2.MemberNumber)
	}
}

func phoneFor(i int) string {
	return "+5244400000" + []string{"01", "02", "03", "04", "05", "06"}[i%6]
}

// ---------------------------------------------------------------------------
// UC-012 / UC-013 — gender (DA-012.7)
// ---------------------------------------------------------------------------

// TestUC012_CreateMember_WithGender — happy path con género válido +
// round-trip por el repo SQLite + edición que limpia el campo.
func TestUC012_CreateMember_WithGender(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	create := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	mujer := memberDomain.GenderFemale
	out, err := create.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "María", Phone: "+524421234567",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
		Gender: &mujer,
	})
	if err != nil {
		t.Fatalf("create with mujer: %v", err)
	}
	// Round-trip: leer del repo y validar.
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	got, err := f.memberRepo.GetByID(tx, out.MemberID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Gender == nil || *got.Gender != memberDomain.GenderFemale {
		t.Errorf("Gender persisted = %v, want mujer", got.Gender)
	}

	// Update con "" debe limpiar.
	update := memApp.NewUpdateMember(f.memberRepo, f.uow, f.recorder)
	clear := ""
	if _, err := update.Execute(context.Background(), memApp.UpdateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: out.MemberID,
		Gender: &clear,
	}); err != nil {
		t.Fatalf("clear gender: %v", err)
	}
	tx2, _ := f.uow.Query(context.Background())
	got2, _ := f.memberRepo.GetByID(tx2, out.MemberID)
	if got2.Gender != nil {
		t.Errorf("Gender after clear = %v, want nil", *got2.Gender)
	}
}

// TestUC012_CreateMember_InvalidGender — un valor fuera del enum debe
// rechazar el alta entera con ErrInvalidGender envuelto en ValidationError.
func TestUC012_CreateMember_InvalidGender(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	create := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	bad := "otro"
	_, err := create.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Test", Phone: "+524421231111",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
		Gender: &bad,
	})
	if err == nil {
		t.Fatalf("invalid gender should fail")
	}
	if !strings.Contains(err.Error(), "género") {
		t.Errorf("expected ErrInvalidGender, got %v", err)
	}
}

// TestUC013_UpdateMember_GenderNotProvided_DoesNotTouch — semántica
// "no enviado = no tocar" para el PATCH (consistente con el resto del struct).
func TestUC013_UpdateMember_GenderNotProvided_DoesNotTouch(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	create := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	hombre := memberDomain.GenderMale
	out, err := create.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Pedro", Phone: "+524421232222",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
		Gender: &hombre,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	update := memApp.NewUpdateMember(f.memberRepo, f.uow, f.recorder)
	newName := "Pedro García"
	if _, err := update.Execute(context.Background(), memApp.UpdateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: out.MemberID,
		FullName: &newName, // Gender intencionalmente nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	tx, _ := f.uow.Query(context.Background())
	got, _ := f.memberRepo.GetByID(tx, out.MemberID)
	if got.Gender == nil || *got.Gender != memberDomain.GenderMale {
		t.Errorf("Gender mutated when not provided: %v", got.Gender)
	}
}

// fakeReceiptNotifier graba las invocaciones del seam del recibo del primer
// pago, para verificar que CreateMember lo dispara (regresión: el alta cobraba
// el primer pago pero nunca encolaba el recibo — sí lo hacía la renovación).
type fakeReceiptNotifier struct {
	calls []memApp.PaymentReceiptInput
}

func (f *fakeReceiptNotifier) NotifyPaymentReceipt(_ context.Context, in memApp.PaymentReceiptInput) {
	f.calls = append(f.calls, in)
}

func TestUC012_CreateMember_FirstPaymentEnqueuesReceipt(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")

	paymentRepo := billingRepoLite.NewPaymentSQLiteRepository()
	folios := folioSvc.NewGenerator(paymentRepo)
	receipt := &fakeReceiptNotifier{}
	uc := memApp.NewCreateMemberWithBilling(f.memberRepo, f.membershipR, f.mtRepo, paymentRepo, folios, f.uow, f.recorder).
		WithReceiptNotifier(receipt)

	out, err := uc.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName:           "Ana López",
		Phone:              "+524429876543",
		MembershipTypeID:   typeID,
		StartDate:          time.Now().UTC(),
		ChargeFirstPayment: true,
		PaymentMethod:      "cash",
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	if len(receipt.calls) != 1 {
		t.Fatalf("receipt notifier llamado %d veces, want 1 (el recibo del primer pago no se encoló)", len(receipt.calls))
	}
	got := receipt.calls[0]
	if out.PaymentID == nil || got.PaymentID != *out.PaymentID {
		t.Errorf("receipt PaymentID = %v, want %v", got.PaymentID, out.PaymentID)
	}
	if got.MemberID != out.MemberID {
		t.Errorf("receipt MemberID = %v, want %v", got.MemberID, out.MemberID)
	}
	if got.MembershipTypeName != "Mensual" {
		t.Errorf("receipt MembershipTypeName = %q, want Mensual", got.MembershipTypeName)
	}
	if got.Amount <= 0 {
		t.Errorf("receipt Amount = %v, want > 0", got.Amount)
	}
	if got.NewExpiry == nil {
		t.Errorf("receipt NewExpiry nil — debería traer la vigencia nueva del primer pago")
	}
}

func TestUC012_CreateMember_NoFirstPayment_NoReceipt(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")

	paymentRepo := billingRepoLite.NewPaymentSQLiteRepository()
	folios := folioSvc.NewGenerator(paymentRepo)
	receipt := &fakeReceiptNotifier{}
	uc := memApp.NewCreateMemberWithBilling(f.memberRepo, f.membershipR, f.mtRepo, paymentRepo, folios, f.uow, f.recorder).
		WithReceiptNotifier(receipt)

	if _, err := uc.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName:           "Sin Pago",
		Phone:              "+524420000000",
		MembershipTypeID:   typeID,
		StartDate:          time.Now().UTC(),
		ChargeFirstPayment: false,
	}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	if len(receipt.calls) != 0 {
		t.Errorf("receipt notifier llamado %d veces sin primer pago, want 0", len(receipt.calls))
	}
}
