//go:build server && integration

package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	gymRepos "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func TestLaunchAccessCloudAuthorityAndSync(t *testing.T) {
	db := projectorTestDB(t)
	gymID, _ := seedGymAndOwner(t, db)
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	ctx := context.Background()
	store := NewPostgresStore()
	// Even a forged push must not grant access or change the billed SKU.
	payload := mustJSON(t, map[string]any{"id": gymID.String(), "gym_id": gymID.String(), "name": "Edited on desktop", "version": 2, "updated_at": time.Now().UnixMilli(), "all_features_access": true, "subscription_plan": "plus_annual", "subscription_status": "active"})
	if err := projectors["gyms"](db, gymID, gymID, payload); err != nil {
		t.Fatal(err)
	}
	tx, err := uow.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo := gymRepos.NewGymPostgresRepository()
	gym, err := repo.GetByID(tx, gymID)
	if err != nil {
		t.Fatal(err)
	}
	if gym.AllFeaturesAccess || gym.SubscriptionPlan != "trial" {
		t.Fatal("client changed cloud entitlements")
	}
	if err := db.Exec(`INSERT INTO sync_entities (gym_id,entity_type,entity_id,version,payload,server_updated_at) VALUES (?,'gyms',?,2,?::jsonb,NOW()-INTERVAL '1 hour')`, gymID, gymID, string(payload)).Error; err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Second)
	// Simulate a cloud grant; ordinary profile saves cannot clear it from a stale aggregate.
	if err := db.Exec(`UPDATE gyms SET all_features_access=TRUE WHERE id=?`, gymID).Error; err != nil {
		t.Fatal(err)
	}
	if err := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		if _, err := repo.Update(tx, gym); err != nil {
			return err
		}
		return store.TouchGym(ctx, tx, gymID)
	}); err != nil {
		t.Fatal(err)
	}
	tx, err = uow.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changes, _, err := store.ListSince(ctx, tx, gymID, before, 100)
	if err != nil {
		t.Fatal(err)
	}
	check := func(changes []PullChange) {
		t.Helper()
		found := false
		for _, ch := range changes {
			if ch.EntityType == "gyms" && ch.EntityID == gymID.String() {
				var pl map[string]any
				if err := json.Unmarshal(ch.Payload, &pl); err != nil {
					t.Fatal(err)
				}
				if pl["all_features_access"] != true || pl["subscription_plan"] != "trial" {
					t.Fatalf("noncanonical grant: %s", ch.Payload)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("grant missing from sync")
		}
	}
	check(changes)
	changes, _, _, err = store.ListForFullSync(ctx, tx, gymID, FullCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	check(changes)
	// A later false from desktop also cannot revoke a server grant.
	payload = mustJSON(t, map[string]any{"id": gymID.String(), "gym_id": gymID.String(), "version": 8, "name": "Second offline edit", "updated_at": time.Now().UnixMilli(), "all_features_access": false})
	if err := projectors["gyms"](db, gymID, gymID, payload); err != nil {
		t.Fatal(err)
	}
	gym, err = repo.GetByID(tx, gymID)
	if err != nil {
		t.Fatal(err)
	}
	if !gym.AllFeaturesAccess {
		t.Fatal("desktop revoked grant")
	}
}

func TestLaunchTouchAfterOlderTransactionRemainsVisible(t *testing.T) {
	db := projectorTestDB(t)
	gymID, _ := seedGymAndOwner(t, db)
	if err := db.Exec(`INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at) VALUES(?,'gyms',?,1,'{}',NOW()-INTERVAL '1 hour')`, gymID, gymID).Error; err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	defer tx.Rollback()
	// Establish transaction time, then simulate an incremental cursor advancing
	// while the payment transaction is waiting to write the grant.
	var started time.Time
	if err := tx.Raw(`SELECT NOW()`).Row().Scan(&started); err != nil {
		t.Fatal(err)
	}
	var cursor time.Time
	if err := db.Raw(`SELECT clock_timestamp()`).Row().Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if !cursor.After(started) {
		t.Fatal("invalid clock fixture")
	}
	store := NewPostgresStore()
	if err := store.TouchGym(context.Background(), &sharedDomain.GormTransaction{Tx: tx}, gymID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	changes, _, err := store.ListSince(context.Background(), &sharedDomain.GormTransaction{Tx: db}, gymID, cursor, 10)
	if err != nil || len(changes) != 1 {
		t.Fatalf("grant skipped by cursor: changes=%d err=%v", len(changes), err)
	}
}
