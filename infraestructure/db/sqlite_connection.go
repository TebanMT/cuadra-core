//go:build sidecar

package db

import (
	"fmt"
	"log"
	"sync"

	"github.com/jmoiron/sqlx"

	// Driver registration — required for sqlite3.
	_ "github.com/mattn/go-sqlite3"
)

var (
	sqliteInstance *sqlx.DB
	sqliteOnce     sync.Once
)

// InitSQLite opens the local SQLite file with foreign keys + WAL enabled.
// path is typically `${HOME}/.cuadra/data/cuadra.db` in production; tests
// use a tmp path.
func InitSQLite(path string) *sqlx.DB {
	sqliteOnce.Do(func() {
		dsn := fmt.Sprintf("%s?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000", path)
		db, err := sqlx.Open("sqlite3", dsn)
		if err != nil {
			log.Fatalf("sqlite open: %v", err)
		}
		// Single writer — SQLite handles concurrent reads fine but only one
		// writer at a time. Setting MaxOpenConns=1 sidesteps SQLITE_BUSY at
		// our scale (one sidecar, one frontend).
		db.SetMaxOpenConns(1)
		if err := db.Ping(); err != nil {
			log.Fatalf("sqlite ping: %v", err)
		}
		sqliteInstance = db
	})
	return sqliteInstance
}

func CloseSQLite() {
	if sqliteInstance != nil {
		_ = sqliteInstance.Close()
	}
}
