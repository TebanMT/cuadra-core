//go:build sidecar

package app_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// TestSignupOwner_SQLite hits a real SQLite file (in tmp) end-to-end. We use
// the sidecar build tag because the repo + UoW + sync queue are tagged.
func TestSignupOwner_SQLite(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	dsn := dbPath + "?_foreign_keys=on"
	db, err := sqlx.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	for _, m := range []string{
		"../../../../db_migrations/sqlite/001_init_schema.sql",
		"../../../../db_migrations/sqlite/005_users_pin.sql",
		"../../../../db_migrations/sqlite/008_gym_charge_settings.sql",
		"../../../../db_migrations/sqlite/018_gyms_stripe_customer.sql",
	} {
		schema, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(schema)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}

	uow := sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue())
	uc := usersApp.NewSignupOwner(
		usersRepoLite.NewUserSQLiteRepository(),
		gymRepoLite.NewGymSQLiteRepository(),
		uow,
		auth.NewJWTService("test-secret"),
		audit.NewSQLiteRecorder(),
		30,
	)
	out, err := uc.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName:        "Esteban Mares",
		Email:           "esteban@gym.com",
		Password:        "supersecret123",
		PasswordConfirm: "supersecret123",
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	if out.UserID.String() == "" || out.GymID.String() == "" || out.AccessToken == "" {
		t.Errorf("missing fields in output: %+v", out)
	}
	// El dueño recibe un PIN auto-generado al alta. Debe ser de 4 dígitos y
	// llegar plaintext en la respuesta (la única vez que viaja en claro).
	if len(out.PIN) != 4 {
		t.Errorf("expected 4-digit PIN in output, got %q", out.PIN)
	}
	for _, r := range out.PIN {
		if r < '0' || r > '9' {
			t.Errorf("PIN should be numeric, got %q", out.PIN)
			break
		}
	}
	// Y el row del owner debe quedar con pin_hash poblado (NUNCA el plaintext).
	var pinHash *string
	if err := db.Get(&pinHash, "SELECT pin_hash FROM users WHERE id = ?", out.UserID.String()); err != nil {
		t.Fatalf("query pin_hash: %v", err)
	}
	if pinHash == nil || *pinHash == "" {
		t.Errorf("owner pin_hash should be set after signup")
	}
	if pinHash != nil && *pinHash == out.PIN {
		t.Errorf("pin_hash stored plaintext PIN; should be bcrypt-hashed")
	}

	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM users"); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 1 {
		t.Errorf("users count = %d, want 1", n)
	}
	if err := db.Get(&n, "SELECT COUNT(*) FROM gyms"); err != nil {
		t.Fatalf("count gyms: %v", err)
	}
	if n != 1 {
		t.Errorf("gyms count = %d, want 1", n)
	}
	// Audit row + one sync_queue row per write
	if err := db.Get(&n, "SELECT COUNT(*) FROM audit_log"); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 1 {
		t.Errorf("audit_log count = %d, want 1", n)
	}
	if err := db.Get(&n, "SELECT COUNT(*) FROM sync_queue WHERE entity_type = 'users'"); err != nil {
		t.Fatalf("count sync users: %v", err)
	}
	if n != 1 {
		t.Errorf("sync_queue users count = %d, want 1", n)
	}

	// Duplicate signup should fail with business error.
	_, err = uc.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName:        "Esteban Otro",
		Email:           "esteban@gym.com",
		Password:        "anotherone1",
		PasswordConfirm: "anotherone1",
	})
	if err == nil {
		t.Errorf("expected duplicate-email error")
	}
}

// TestSignupOwner_WithPhone — phone is optional, but when supplied it must
// land in the users row exactly as the user typed it (post-trim). The
// scenario that justifies separate test coverage is a future WhatsApp
// recovery flow that needs the stored phone to match the number the owner
// will key in to receive the OTP — silent normalization would break that.
func TestSignupOwner_WithPhone(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	dsn := dbPath + "?_foreign_keys=on"
	db, err := sqlx.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	for _, m := range []string{
		"../../../../db_migrations/sqlite/001_init_schema.sql",
		"../../../../db_migrations/sqlite/005_users_pin.sql",
		"../../../../db_migrations/sqlite/008_gym_charge_settings.sql",
		"../../../../db_migrations/sqlite/018_gyms_stripe_customer.sql",
	} {
		schema, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(schema)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}

	uow := sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue())
	uc := usersApp.NewSignupOwner(
		usersRepoLite.NewUserSQLiteRepository(),
		gymRepoLite.NewGymSQLiteRepository(),
		uow,
		auth.NewJWTService("test-secret"),
		audit.NewSQLiteRecorder(),
		30,
	)
	// El dueño teclea con espacios y sin código de país; debe quedar E.164.
	const inputPhone = "55 1234 5678"
	const wantPhone = "+525512345678"
	if _, err := uc.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName:        "Esteban Mares",
		Email:           "esteban@gym.com",
		Phone:           inputPhone,
		Password:        "supersecret123",
		PasswordConfirm: "supersecret123",
	}); err != nil {
		t.Fatalf("signup with phone: %v", err)
	}
	var got string
	if err := db.Get(&got, "SELECT phone FROM users WHERE email = 'esteban@gym.com'"); err != nil {
		t.Fatalf("read phone: %v", err)
	}
	if got != wantPhone {
		t.Errorf("phone = %q, want %q (normalizado E.164)", got, wantPhone)
	}

	// Invalid phone (letters) must reject the whole signup — we cannot
	// silently strip "abc" because it changes user intent.
	_, err = uc.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName:        "Otro Owner",
		Email:           "otro@gym.com",
		Phone:           "abc-letters",
		Password:        "anotherone1",
		PasswordConfirm: "anotherone1",
	})
	if err == nil {
		t.Errorf("expected validation error for letters in phone")
	}
}
