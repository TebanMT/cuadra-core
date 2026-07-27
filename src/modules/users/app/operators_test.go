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

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// applySQLiteMigrations carga el subset mínimo de migraciones que necesita
// el test. Mismo patrón que migrate_sqlite_test.go: las migraciones 005-012
// quedaron consolidadas dentro de 001 (refactor), así que aplicarlas todas
// en un DB fresco truena con "duplicate column name". Para los tests de
// PIN-first basta con 001 + 008 (gyms.charge_settings) + 013/015 (recreos
// de gyms) + nuestra 016.
func applySQLiteMigrations(t *testing.T, dir string, db *sqlx.DB) {
	t.Helper()
	// Mismo subset que members_integration_test.go + nuestra 016 nueva.
	// Las migraciones intermedias quedaron consolidadas en 001 — aplicarlas
	// truena con "duplicate column name".
	for _, m := range []string{
		"001_init_schema.sql",
		"005_users_pin.sql",
		"008_gym_charge_settings.sql",
		"016_operators_pin_first.sql",
		"018_gyms_stripe_customer.sql",
	} {
		raw, err := os.ReadFile(filepath.Join(dir, m))
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(raw)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}
}

func setupOperatorsFixture(t *testing.T) (*sqlx.DB, sharedDomain.UnitOfWork, *gymDomain.Gym) {
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

	applySQLiteMigrations(t, "../../../../db_migrations/sqlite", db)

	uow := sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue())
	gymRepo := gymRepoLite.NewGymSQLiteRepository()

	gym := gymDomain.NewTrialGym(uuid.New(), 30, time.Now().UTC())
	name := "Gym de Prueba"
	gym.Name = &name
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := gymRepo.Create(tx, gym)
		return err
	}); err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	return db, uow, gym
}

func TestCreateOperator_HappyPath_NoWhatsApp(t *testing.T) {
	db, uow, gym := setupOperatorsFixture(t)
	userRepo := usersRepoLite.NewUserSQLiteRepository()
	rec := audit.NewSQLiteRecorder()

	// Owner row es prerequisito para que CountOperatorsByGym tenga sentido.
	seedOwner(t, uow, userRepo, gym.ID)

	uc := usersApp.NewCreateOperator(userRepo, uow, rec)
	out, err := uc.Execute(context.Background(), usersApp.CreateOperatorInput{
		GymID:    gym.ID,
		OwnerID:  fetchOwnerID(t, db, gym.ID),
		FullName: "Carla Recepción",
		Phone:    "+52 442 123 4567",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if out.PIN == "" || len(out.PIN) != 4 {
		t.Errorf("expected 4-digit pin, got %q", out.PIN)
	}
	if out.WhatsAppDelivery {
		t.Errorf("no notifier wired -> WhatsAppDelivery should be false")
	}
	if out.WhatsAppSkipped == "" {
		t.Errorf("expected skip reason when notifier not wired")
	}

	// Asegura que pin_hash quedó set y password_hash NULL.
	var pinHash, pwdHash *string
	if err := db.Get(&pinHash, "SELECT pin_hash FROM users WHERE id = ?", out.UserID.String()); err != nil {
		t.Fatalf("query pin_hash: %v", err)
	}
	if pinHash == nil || *pinHash == "" {
		t.Errorf("pin_hash should be set")
	}
	if err := db.Get(&pwdHash, "SELECT password_hash FROM users WHERE id = ?", out.UserID.String()); err != nil {
		t.Fatalf("query password_hash: %v", err)
	}
	if pwdHash != nil && *pwdHash != "" {
		t.Errorf("password_hash should be NULL for PIN-first operator; got %q", *pwdHash)
	}

	// Audit row no debe contener el PIN plaintext.
	var changes string
	if err := db.Get(&changes, "SELECT COALESCE(changes,'') FROM audit_log WHERE entity_id = ?", out.UserID.String()); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if strings.Contains(changes, out.PIN) {
		t.Errorf("audit log should NOT contain PIN plaintext; got %q", changes)
	}
}

func TestCreateOperator_PhoneRequired(t *testing.T) {
	_, uow, gym := setupOperatorsFixture(t)
	userRepo := usersRepoLite.NewUserSQLiteRepository()
	rec := audit.NewSQLiteRecorder()
	seedOwner(t, uow, userRepo, gym.ID)

	uc := usersApp.NewCreateOperator(userRepo, uow, rec)
	_, err := uc.Execute(context.Background(), usersApp.CreateOperatorInput{
		GymID:    gym.ID,
		OwnerID:  uuid.New(),
		FullName: "Carla Recepción",
		Phone:    "",
	})
	if err == nil {
		t.Fatalf("expected ErrOperatorPhoneRequired, got nil")
	}
	if !strings.Contains(err.Error(), userErrors.ErrOperatorPhoneRequired.Error()) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCreateOperator_PhoneUniquenessPerGym(t *testing.T) {
	db, uow, gym := setupOperatorsFixture(t)
	userRepo := usersRepoLite.NewUserSQLiteRepository()
	rec := audit.NewSQLiteRecorder()
	seedOwner(t, uow, userRepo, gym.ID)
	ownerID := fetchOwnerID(t, db, gym.ID)

	uc := usersApp.NewCreateOperator(userRepo, uow, rec)
	if _, err := uc.Execute(context.Background(), usersApp.CreateOperatorInput{
		GymID: gym.ID, OwnerID: ownerID,
		FullName: "Carla Recepción", Phone: "4421234567",
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Mismo gym, mismo número (con separadores diferentes) -> debe colisionar.
	_, err := uc.Execute(context.Background(), usersApp.CreateOperatorInput{
		GymID: gym.ID, OwnerID: ownerID,
		FullName: "Otra persona", Phone: "(442) 123-4567",
	})
	if err == nil || !strings.Contains(err.Error(), userErrors.ErrPhoneTakenInGym.Error()) {
		t.Errorf("expected ErrPhoneTakenInGym, got %v", err)
	}
}

func TestRotateOperatorPIN_RewritesHash(t *testing.T) {
	db, uow, gym := setupOperatorsFixture(t)
	userRepo := usersRepoLite.NewUserSQLiteRepository()
	rec := audit.NewSQLiteRecorder()
	seedOwner(t, uow, userRepo, gym.ID)
	ownerID := fetchOwnerID(t, db, gym.ID)

	create := usersApp.NewCreateOperator(userRepo, uow, rec)
	out, err := create.Execute(context.Background(), usersApp.CreateOperatorInput{
		GymID: gym.ID, OwnerID: ownerID,
		FullName: "Carla", Phone: "4425551234",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var before string
	if err := db.Get(&before, "SELECT pin_hash FROM users WHERE id = ?", out.UserID.String()); err != nil {
		t.Fatalf("before pin_hash: %v", err)
	}

	rotate := usersApp.NewRotateOperatorPIN(userRepo, uow, rec)
	rotateOut, err := rotate.Execute(context.Background(), usersApp.RotateOperatorPINInput{
		GymID: gym.ID, ActorUserID: ownerID, TargetID: out.UserID,
	})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotateOut.PIN == out.PIN {
		t.Errorf("rotate should produce a new PIN")
	}
	var after string
	if err := db.Get(&after, "SELECT pin_hash FROM users WHERE id = ?", out.UserID.String()); err != nil {
		t.Fatalf("after pin_hash: %v", err)
	}
	if after == before {
		t.Errorf("pin_hash should have changed")
	}
}

func TestRotateOperatorPIN_RejectsOwner(t *testing.T) {
	db, uow, gym := setupOperatorsFixture(t)
	userRepo := usersRepoLite.NewUserSQLiteRepository()
	rec := audit.NewSQLiteRecorder()
	seedOwner(t, uow, userRepo, gym.ID)
	ownerID := fetchOwnerID(t, db, gym.ID)

	rotate := usersApp.NewRotateOperatorPIN(userRepo, uow, rec)
	_, err := rotate.Execute(context.Background(), usersApp.RotateOperatorPINInput{
		GymID: gym.ID, ActorUserID: ownerID, TargetID: ownerID,
	})
	if err == nil || !strings.Contains(err.Error(), userErrors.ErrTargetNotEligible.Error()) {
		t.Errorf("expected ErrTargetNotEligible, got %v", err)
	}
}

func TestResetOperatorPassword_RejectsOwner(t *testing.T) {
	db, uow, gym := setupOperatorsFixture(t)
	userRepo := usersRepoLite.NewUserSQLiteRepository()
	rec := audit.NewSQLiteRecorder()
	seedOwner(t, uow, userRepo, gym.ID)
	ownerID := fetchOwnerID(t, db, gym.ID)

	reset := usersApp.NewResetOperatorPassword(userRepo, nil, uow, rec)
	_, err := reset.Execute(context.Background(), usersApp.ResetOperatorPasswordInput{
		GymID: gym.ID, ActorUserID: ownerID, TargetID: ownerID,
	})
	if err == nil || !strings.Contains(err.Error(), userErrors.ErrTargetNotEligible.Error()) {
		t.Errorf("apuntar al dueño debe dar ErrTargetNotEligible, got %v", err)
	}
	// El password_hash del dueño debe quedar intacto — el password del dueño
	// es autoridad del cloud, no se toca desde este endpoint legacy.
	var pwdHash *string
	if err := db.Get(&pwdHash, "SELECT password_hash FROM users WHERE id = ?", ownerID.String()); err != nil {
		t.Fatalf("query password_hash: %v", err)
	}
	if pwdHash == nil || *pwdHash != "bcrypt-hash" {
		t.Errorf("password_hash del dueño debe quedar intacto ('bcrypt-hash'); got %v", pwdHash)
	}
}

func TestResetOperatorPassword_AllowsOperator(t *testing.T) {
	db, uow, gym := setupOperatorsFixture(t)
	userRepo := usersRepoLite.NewUserSQLiteRepository()
	rec := audit.NewSQLiteRecorder()
	seedOwner(t, uow, userRepo, gym.ID)
	ownerID := fetchOwnerID(t, db, gym.ID)

	create := usersApp.NewCreateOperator(userRepo, uow, rec)
	out, err := create.Execute(context.Background(), usersApp.CreateOperatorInput{
		GymID: gym.ID, OwnerID: ownerID,
		FullName: "Carla", Phone: "4425551234",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	reset := usersApp.NewResetOperatorPassword(userRepo, nil, uow, rec)
	rout, err := reset.Execute(context.Background(), usersApp.ResetOperatorPasswordInput{
		GymID: gym.ID, ActorUserID: ownerID, TargetID: out.UserID,
	})
	if err != nil {
		t.Fatalf("reset de operador debe pasar, got %v", err)
	}
	if rout.Password == "" {
		t.Errorf("esperaba una contraseña temporal no vacía")
	}
	// El operador queda con password_hash set (login legacy email+password).
	var pwdHash *string
	if err := db.Get(&pwdHash, "SELECT password_hash FROM users WHERE id = ?", out.UserID.String()); err != nil {
		t.Fatalf("query password_hash: %v", err)
	}
	if pwdHash == nil || *pwdHash == "" {
		t.Errorf("el operador debe quedar con password_hash tras el reset")
	}
}

// seedOwner crea un owner directamente vía dominio + repo, sin pasar por
// SignupOwner (que requiere wireup de JWT). Suficiente para CountOperatorsByGym
// y para satisfacer el created_by FK.
func seedOwner(t *testing.T, uow sharedDomain.UnitOfWork, repo *usersRepoLite.UserSQLiteRepository, gymID uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()
	owner, err := userDomain.NewOwner(uuid.New(), gymID, "owner@test.com", "bcrypt-hash", "Owner", false, now)
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := repo.Create(tx, owner)
		return err
	}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
}

func fetchOwnerID(t *testing.T, db *sqlx.DB, gymID uuid.UUID) uuid.UUID {
	t.Helper()
	var idStr string
	if err := db.Get(&idStr, "SELECT id FROM users WHERE gym_id = ? AND role = 'owner'", gymID.String()); err != nil {
		t.Fatalf("fetch owner: %v", err)
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		t.Fatalf("parse uuid: %v", err)
	}
	return id
}

// TestOperatorsGrid_LoginPIN_ToleranPasswordHashNull: regresión del scan.
// password_hash es NULLABLE desde la migración 016 y el pull del cloud puede
// escribir NULL (payload canónico con password_hash: null). Con el row struct
// en `string` a secas, UNA fila NULL tumbaba el SELECT * completo: el grid de
// /auth/operators moría con "sql: Scan error on column index 8" y el
// login-pin del usuario afectado (el OWNER en el gym piloto) devolvía
// "PIN incorrecto" opaco sin siquiera evaluar el PIN.
func TestOperatorsGrid_LoginPIN_ToleranPasswordHashNull(t *testing.T) {
	db, uow, gym := setupOperatorsFixture(t)
	userRepo := usersRepoLite.NewUserSQLiteRepository()
	rec := audit.NewSQLiteRecorder()

	seedOwner(t, uow, userRepo, gym.ID)
	ownerID := fetchOwnerID(t, db, gym.ID)

	// Owner con PIN asignado (flujo installer/welcome).
	pinHash, err := auth.HashPIN("4321")
	if err != nil {
		t.Fatalf("hash pin: %v", err)
	}
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		owner, gerr := userRepo.GetByID(tx, ownerID)
		if gerr != nil {
			return gerr
		}
		owner.AssignPIN(pinHash, time.Now().UTC())
		_, uerr := userRepo.Update(tx, owner)
		return uerr
	}); err != nil {
		t.Fatalf("assign pin: %v", err)
	}

	// Simular el pull que deja password_hash en NULL (la fila del owner en
	// el gym piloto tras el round-trip de la era de la corrupción jun-2026).
	if _, err := db.Exec("UPDATE users SET password_hash = NULL WHERE id = ?", ownerID.String()); err != nil {
		t.Fatalf("null password_hash: %v", err)
	}

	// (a) El grid de login no debe morir por la fila NULL.
	list := usersApp.NewListOperatorsForLogin(userRepo, uow)
	ops, err := list.Execute(context.Background(), gym.ID)
	if err != nil {
		t.Fatalf("el grid de operadores debe tolerar password_hash NULL: %v", err)
	}
	found := false
	for _, op := range ops {
		if op.UserID == ownerID {
			found = true
			if !op.HasPIN {
				t.Errorf("owner debe reportar has_pin=true")
			}
		}
	}
	if !found {
		t.Fatalf("owner ausente del grid: %+v", ops)
	}

	// (b) El login-pin del owner debe funcionar — antes fallaba en GetByID
	// (scan) y salía como "PIN incorrecto" sin evaluar el PIN.
	login := usersApp.NewLoginPIN(userRepo, gymRepoLite.NewGymSQLiteRepository(), uow,
		auth.NewJWTService("test-secret"), rec)
	out, err := login.Execute(context.Background(), usersApp.LoginPINInput{
		UserID: ownerID, PIN: "4321",
	})
	if err != nil {
		t.Fatalf("login-pin del owner con password_hash NULL: %v", err)
	}
	if out.Role != "owner" || out.AccessToken == "" {
		t.Errorf("login-pin incompleto: %+v", out)
	}
}
