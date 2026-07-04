//go:build server

// Package testutil holds shared helpers for the server integration tests.
// It is only ever imported from *_integration_test.go files; nothing in
// production code should depend on it.
package testutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	infraDB "github.com/cuadra/cuadra-core/infraestructure/db"
)

const defaultPostgresDSN = "postgres://postgres:postgres@localhost:5432/cuadra?sslmode=disable"

// migrationsLockKey serializes migration application across test binaries:
// `go test ./...` runs packages in parallel, and two processes applying the
// same migration to an empty database race on CREATE TABLE. Arbitrary but
// stable — 0x74696e7461 spells "tinta".
const migrationsLockKey = 0x74696e7461

var (
	migrateOnce sync.Once
	migrateErr  error
)

// OpenPostgres opens the integration Postgres handle (DATABASE_URL, falling
// back to the local default) and guarantees the schema is current by running
// ApplyPostgresMigrations before returning. Skips the test when Postgres is
// unreachable so CI on machines without Postgres stays green; fails it when
// Postgres is up but the migrations themselves error.
//
// Migrations run once per test binary (sync.Once) and under a Postgres
// advisory lock so concurrent packages don't stampede an empty database —
// the runner is idempotent via _migrations, so waiters no-op.
func OpenPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultPostgresDSN
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Skipf("integration test skipped — cannot reach Postgres at %s: %v", dsn, err)
	}
	migrateOnce.Do(func() { migrateErr = applyMigrations(db) })
	if migrateErr != nil {
		t.Fatalf("apply postgres migrations: %v", migrateErr)
	}
	return db
}

func applyMigrations(db *gorm.DB) error {
	dir, err := migrationsDir()
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	// The advisory lock is session-scoped, so it lives on its own pinned
	// *sql.Conn; the migrations themselves run through the regular gorm
	// pool exactly like the server-boot path does. The lock is just a
	// cross-process mutex — it doesn't need to share a session with the
	// DDL, and closing the conn releases it even on a panic.
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin migrations-lock conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", int64(migrationsLockKey)); err != nil {
		return fmt.Errorf("acquire migrations lock: %w", err)
	}
	defer conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", int64(migrationsLockKey))
	return infraDB.ApplyPostgresMigrations(db, dir)
}

// migrationsDir resolves db_migrations/postgres relative to the repo root,
// not the cwd — `go test` runs each package from its own directory. The root
// is found by walking up from this source file until go.mod appears.
func migrationsDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("resolve migrations dir: runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "db_migrations", "postgres"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("resolve migrations dir: no go.mod above %s", filepath.Dir(file))
		}
		dir = parent
	}
}
