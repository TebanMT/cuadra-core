//go:build sidecar

package repositories

import (
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
	"testing"
	"time"
)

func TestLastEntry_SQLite(t *testing.T) {
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE checkins (gym_id TEXT, member_id TEXT, checkin_at INTEGER, result TEXT, deleted_at INTEGER)`)
	if err != nil {
		t.Fatal(err)
	}
	tx := &shared.SqlxTransaction{DB: db}
	repo := NewCheckinSQLiteRepository()
	verifyLastEntry(t, uuid.New(), uuid.New(), uuid.New(), uuid.New(), func(g, m uuid.UUID, at time.Time, result string, deleted bool) {
		var deletedAt any
		if deleted {
			deletedAt = at.UnixMilli()
		}
		if _, err := db.Exec(`INSERT INTO checkins VALUES (?,?,?,?,?)`, g.String(), m.String(), at.UnixMilli(), result, deletedAt); err != nil {
			t.Fatal(err)
		}
	}, func(g, m uuid.UUID) (*time.Time, error) { return repo.LastEntryAt(tx, g, m) })
}
