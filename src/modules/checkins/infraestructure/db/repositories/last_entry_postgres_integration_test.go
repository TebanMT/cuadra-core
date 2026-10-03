//go:build server && integration

package repositories

import (
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestLastEntry_Postgres(t *testing.T) {
	db := testutil.OpenPostgres(t).Begin()
	if db.Error != nil {
		t.Fatal(db.Error)
	}
	defer db.Rollback()
	gym, member, otherGym, otherMember := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for g, m := range map[uuid.UUID]uuid.UUID{gym: member, otherGym: otherMember} {
		if err := db.Exec(`INSERT INTO gyms(id,gym_id,name,country,timezone) VALUES (?,?,'Entry fixture','MX','America/Mexico_City')`, g, g).Error; err != nil {
			t.Fatal(err)
		}
		owner := uuid.New()
		if err := db.Exec(`INSERT INTO users(id,gym_id,email,password_hash,full_name,role,active) VALUES (?,?,?,'unused','Fixture owner','owner',TRUE)`, owner, g, owner.String()+"@test.invalid").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`INSERT INTO members(id,gym_id,folio,full_name,phone,created_by) VALUES (?,?,'TEST','Entry member',?,?)`, m, g, m.String(), owner).Error; err != nil {
			t.Fatal(err)
		}
	}
	tx := &shared.GormTransaction{Tx: db}
	repo := NewCheckinPostgresRepository()
	verifyLastEntry(t, gym, member, otherGym, otherMember, func(g, m uuid.UUID, at time.Time, result string, deleted bool) {
		var deletedAt any
		if deleted {
			deletedAt = at
		}
		if err := db.Exec(`INSERT INTO checkins(id,gym_id,member_id,checkin_at,result,method,deleted_at) VALUES (?,?,?,?,?,'manual',?)`, uuid.New(), g, m, at, result, deletedAt).Error; err != nil {
			t.Fatal(err)
		}
	}, func(g, m uuid.UUID) (*time.Time, error) { return repo.LastEntryAt(tx, g, m) })
}
