//go:build server && integration

package repositories

import (
	repo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestHistoryPaging_Postgres(t *testing.T) {
	db := testutil.OpenPostgres(t).Begin()
	if db.Error != nil {
		t.Fatal(db.Error)
	}
	defer db.Rollback()
	// Temporary tables keep the fixture independent of production constraints.
	for _, q := range []string{
		`CREATE TEMP TABLE checkins(id UUID, gym_id UUID, member_id UUID, operator_id UUID, method TEXT, result TEXT, manual_override BOOLEAN, override_reason TEXT, checkin_at TIMESTAMPTZ, deleted_at TIMESTAMPTZ) ON COMMIT DROP`,
		`CREATE TEMP TABLE members(id UUID, full_name TEXT, deleted_at TIMESTAMPTZ) ON COMMIT DROP`,
		`CREATE TEMP TABLE users(id UUID, full_name TEXT, deleted_at TIMESTAMPTZ) ON COMMIT DROP`,
		`CREATE TEMP TABLE memberships(member_id UUID, expiry_date DATE, status TEXT, deleted_at TIMESTAMPTZ) ON COMMIT DROP`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	gym := uuid.New()
	tx := &shared.GormTransaction{Tx: db}
	reader := NewCheckinPostgresRepository()
	day := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	verifyHistoryPaging(t, gym, func(g uuid.UUID, at time.Time, result string, deleted bool) {
		var deletedAt any
		if deleted {
			deletedAt = at
		}
		err := db.Exec(`INSERT INTO checkins(id,gym_id,method,result,manual_override,checkin_at,deleted_at) VALUES (?,?,'manual',?,FALSE,?,?)`, uuid.New(), g, result, at, deletedAt).Error
		if err != nil {
			t.Fatal(err)
		}
	}, func(limit, offset int) ([]repo.RecentCheckinRow, error) {
		return reader.ListByGymBetweenPage(tx, gym, "America/Mexico_City", day, day, limit, offset)
	})
}
