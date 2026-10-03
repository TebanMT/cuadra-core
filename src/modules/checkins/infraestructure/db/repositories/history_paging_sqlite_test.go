//go:build sidecar

package repositories

import (
	repo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
	"testing"
	"time"
)

func TestHistoryPaging_SQLite(t *testing.T) {
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE checkins(id TEXT, gym_id TEXT, member_id TEXT, operator_id TEXT, method TEXT, result TEXT, manual_override INTEGER, override_reason TEXT, checkin_at INTEGER, deleted_at INTEGER);
 CREATE TABLE members(id TEXT, full_name TEXT, deleted_at INTEGER);
 CREATE TABLE users(id TEXT, full_name TEXT, deleted_at INTEGER);
 CREATE TABLE memberships(member_id TEXT, expiry_date TEXT, status TEXT, deleted_at INTEGER);`)
	if err != nil {
		t.Fatal(err)
	}
	gym := uuid.New()
	tx := &shared.SqlxTransaction{DB: db}
	reader := NewCheckinSQLiteRepository()
	day := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	verifyHistoryPaging(t, gym, func(g uuid.UUID, at time.Time, result string, deleted bool) {
		var deletedAt any
		if deleted {
			deletedAt = at.UnixMilli()
		}
		_, err := db.Exec(`INSERT INTO checkins(id,gym_id,method,result,manual_override,checkin_at,deleted_at) VALUES (?,?,'manual',?,0,?,?)`, uuid.NewString(), g.String(), result, at.UnixMilli(), deletedAt)
		if err != nil {
			t.Fatal(err)
		}
	}, func(limit, offset int) ([]repo.RecentCheckinRow, error) {
		return reader.ListByGymBetweenPage(tx, gym, "America/Mexico_City", day, day, limit, offset)
	})
}
