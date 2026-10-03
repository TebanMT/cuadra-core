//go:build server && integration

package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

func userSyncFixture(t *testing.T) (*gorm.DB, shared.UnitOfWork, *userDomain.User) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	gymID := uuid.New()
	if err := db.Exec(`INSERT INTO gyms(id,gym_id,version,created_at,updated_at,name,country,timezone) VALUES(?,?,1,NOW(),NOW(),'Staff sync test','MX','America/Mexico_City')`, gymID, gymID).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"sync_entities", "users", "gyms"} {
			db.Exec("DELETE FROM "+table+" WHERE gym_id=?", gymID)
		}
	})
	now := time.Now().UTC()
	pinHash := "opaque-test-pin-hash"
	u := &userDomain.User{ID: uuid.New(), GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now, FullName: "Recepción", Role: userDomain.RoleOperator, Active: true, PinHash: &pinHash}
	return db, shared.NewPostgresUnitOfWork(db), u
}

func TestCloudOperatorPublishesCreateAndUpdateAtomically(t *testing.T) {
	db, uow, user := userSyncFixture(t)
	repository := NewUserPostgresRepository()
	err := uow.Command(context.Background(), func(tx shared.Transaction) error {
		_, err := repository.Create(tx, user)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var journal struct {
		Version int
		Payload []byte
	}
	if err := db.Raw(`SELECT version,payload FROM sync_entities WHERE gym_id=? AND entity_type='users' AND entity_id=?`, user.GymID, user.ID).Scan(&journal).Error; err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(journal.Payload, &payload); err != nil || payload["pin_hash"] != *user.PinHash || payload["active"] != true {
		t.Fatalf("operator is not available to offline login: %v", err)
	}
	// A journal can be ahead after a prior conflict. Cloud updates must publish
	// a strictly newer version and keep the canonical row at that same version.
	if err := db.Exec(`UPDATE sync_entities SET version=5 WHERE gym_id=? AND entity_type='users' AND entity_id=?`, user.GymID, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	user.Active = false
	user.Version++
	err = uow.Command(context.Background(), func(tx shared.Transaction) error {
		out, err := repository.Update(tx, user)
		if err == nil && out.Version != 6 {
			t.Errorf("returned version=%d", out.Version)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		UserVersion, JournalVersion int
		Active                      bool
	}
	if err := db.Raw(`SELECT u.version AS user_version,se.version AS journal_version,(se.payload->>'active')::boolean AS active FROM users u JOIN sync_entities se ON se.gym_id=u.gym_id AND se.entity_type='users' AND se.entity_id=u.id WHERE u.id=?`, user.ID).Scan(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.UserVersion != 6 || state.JournalVersion != 6 || state.Active {
		t.Fatalf("cloud edit failed to publish canonical state: %+v", state)
	}
}

func TestCloudOperatorRollbackDoesNotPublishPartialUser(t *testing.T) {
	db, uow, user := userSyncFixture(t)
	abort := errors.New("cancel after user write")
	err := uow.Command(context.Background(), func(tx shared.Transaction) error {
		if _, err := NewUserPostgresRepository().Create(tx, user); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	var count int
	if err := db.Raw(`SELECT (SELECT COUNT(*) FROM users WHERE id=?)+(SELECT COUNT(*) FROM sync_entities WHERE gym_id=? AND entity_type='users' AND entity_id=?)`, user.ID, user.GymID, user.ID).Scan(&count).Error; err != nil || count != 0 {
		t.Fatalf("rollback leaked user or feed row: count=%d err=%v", count, err)
	}
}
