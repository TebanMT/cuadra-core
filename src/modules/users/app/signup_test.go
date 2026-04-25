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

	schema, err := os.ReadFile("../../../../db_migrations/sqlite/001_init_schema.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("apply migration: %v", err)
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
