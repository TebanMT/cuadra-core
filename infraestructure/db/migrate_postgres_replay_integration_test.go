//go:build server && integration

package db_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	infraDB "github.com/cuadra/cuadra-core/infraestructure/db"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// CI used to execute the SQL files directly before the tests used the Go
// runner. Missing markers in 055/056/058 made the runner repeat committed DDL.
func TestPostgresDirectSQLThenRunnerDoesNotReplayDDL(t *testing.T) {
	db := emptyMigrationDatabase(t)
	dir := filepath.Join("..", "..", "db_migrations", "postgres")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: files=%d, err=%v", len(files), err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(string(raw)).Error; err != nil {
			t.Fatalf("apply SQL directly %s: %v", filepath.Base(file), err)
		}
		prefix, _, _ := strings.Cut(filepath.Base(file), "_")
		version, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatalf("parse migration version %s: %v", file, err)
		}
		var recorded bool
		if err := db.Raw("SELECT EXISTS (SELECT 1 FROM _migrations WHERE version=?)", version).Scan(&recorded).Error; err != nil {
			t.Fatal(err)
		}
		if !recorded {
			t.Fatalf("direct SQL must record committed migration %s", filepath.Base(file))
		}
	}

	type trigger struct {
		Relation string
		Name     string
		OID      int64
	}
	readTriggers := func() []trigger {
		t.Helper()
		var triggers []trigger
		if err := db.Raw(`SELECT tgrelid::regclass::text AS relation, tgname AS name, oid::bigint AS oid
			FROM pg_trigger WHERE NOT tgisinternal ORDER BY tgrelid, tgname`).Scan(&triggers).Error; err != nil {
			t.Fatal(err)
		}
		return triggers
	}
	before := readTriggers()
	if len(before) == 0 {
		t.Fatal("expected migration-created triggers")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := infraDB.ApplyPostgresMigrations(db, dir); err != nil {
			t.Fatalf("runner attempt %d after direct SQL: %v", attempt, err)
		}
		if after := readTriggers(); !slices.Equal(before, after) {
			t.Fatalf("runner attempt %d recreated or changed existing triggers", attempt)
		}
	}
}

// A separate database keeps this full migration-chain test isolated from the
// fixtures and migration locks used by concurrently running packages.
func emptyMigrationDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	admin := testutil.OpenPostgres(t)
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/cuadra?sslmode=disable"
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse integration PostgreSQL configuration")
	}
	name := "tinta_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec(`CREATE DATABASE "` + name + `"`).Error; err != nil {
		t.Fatalf("create isolated migration database: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.Exec(`DROP DATABASE "` + name + `" WITH (FORCE)`).Error; err != nil {
			t.Errorf("drop isolated migration database: %v", err)
		}
	})
	config.Database = name
	pool := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = pool.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal("open isolated migration database")
	}
	return db
}
