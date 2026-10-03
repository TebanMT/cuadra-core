//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

func TestApplyPullPage_SameNamePendingMembershipTypePreservesBoth(t *testing.T) {
	ctx := context.Background()
	db, uow, gymID := setupSidecarDB(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	localTypeID := uuid.New()
	cloudTypeID := uuid.New()

	if _, err := db.Exec(`
		INSERT INTO membership_types
		    (id, gym_id, version, created_at, updated_at, name, price,
		     duration_days, duration_months, enrollment_fee,
		     maintenance_fee, maintenance_frequency, active)
		VALUES (?, ?, 1, ?, ?, 'Normal', 40000, 30, 1, 0, 0, NULL, 1)`,
		localTypeID, gymID, now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatalf("seed local type: %v", err)
	}
	localPayload := membershipTypePayload(localTypeID, gymID, "Normal", 400, now)
	if err := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		return syncpkg.NewSqliteQueue().Enqueue(
			ctx, tx, "membership_types", localTypeID.String(), "upsert", localPayload, 1,
		)
	}); err != nil {
		t.Fatalf("enqueue local type: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE sync_queue SET retry_count = 16,
		       last_error = 'rejected_duplicate: Normal already exists'
		 WHERE entity_type = 'membership_types' AND entity_id = ?`, localTypeID); err != nil {
		t.Fatalf("seed stuck queue state: %v", err)
	}

	// A local membership already references the pending plan. Recovery must
	// keep this FK on localTypeID; merging IDs/prices would change history.
	memberID := enqueueMember(t, db, gymID, 1)
	membershipID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO memberships
		    (id, gym_id, version, created_at, updated_at, member_id,
		     membership_type_id, type_name_snapshot, price_snapshot,
		     duration_days_snapshot, duration_months_snapshot,
		     start_date, expiry_date, status)
		VALUES (?, ?, 1, ?, ?, ?, ?, 'Normal', 40000, 30, 1,
		        '2026-08-13', '2026-09-13', 'active')`,
		membershipID, gymID, now.UnixMilli(), now.UnixMilli(), memberID, localTypeID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	change := syncpkg.PullChange{
		EntityType:      "membership_types",
		EntityID:        cloudTypeID.String(),
		Version:         1,
		Payload:         membershipTypePayload(cloudTypeID, gymID, "Normal", 450, now),
		ServerUpdatedAt: now.Add(time.Minute),
	}
	if err := syncpkg.ApplyPullPage(ctx, uow, []syncpkg.PullChange{change}, nil); err != nil {
		t.Fatalf("ApplyPullPage: %v", err)
	}

	var localName, cloudName string
	var localPrice, cloudPrice, localVersion int
	if err := db.QueryRowx(`SELECT name, price, version FROM membership_types WHERE id = ?`, localTypeID).
		Scan(&localName, &localPrice, &localVersion); err != nil {
		t.Fatalf("local type: %v", err)
	}
	if err := db.QueryRowx(`SELECT name, price FROM membership_types WHERE id = ?`, cloudTypeID).
		Scan(&cloudName, &cloudPrice); err != nil {
		t.Fatalf("cloud type: %v", err)
	}
	if localName != "Normal (recepción)" || localPrice != 40000 || localVersion != 2 {
		t.Errorf("local = name=%q price=%d version=%d, want Normal (recepción)/40000/2",
			localName, localPrice, localVersion)
	}
	if cloudName != "Normal" || cloudPrice != 45000 {
		t.Errorf("cloud = name=%q price=%d, want Normal/45000", cloudName, cloudPrice)
	}

	var referencedType string
	if err := db.Get(&referencedType, `SELECT membership_type_id FROM memberships WHERE id = ?`, membershipID); err != nil {
		t.Fatalf("membership FK: %v", err)
	}
	if referencedType != localTypeID.String() {
		t.Errorf("membership_type_id = %s, want local %s", referencedType, localTypeID)
	}

	var queuedPayload string
	var queuedVersion, retryCount int
	var lastError *string
	if err := db.QueryRowx(`
		SELECT payload, client_version, retry_count, last_error
		  FROM sync_queue
		 WHERE entity_type = 'membership_types' AND entity_id = ? AND synced_at IS NULL`, localTypeID).
		Scan(&queuedPayload, &queuedVersion, &retryCount, &lastError); err != nil {
		t.Fatalf("rewritten queue: %v", err)
	}
	var queued map[string]any
	_ = json.Unmarshal([]byte(queuedPayload), &queued)
	if queued["name"] != "Normal (recepción)" || int(queued["version"].(float64)) != 2 {
		t.Errorf("queued payload = %+v", queued)
	}
	if queuedVersion != 2 || retryCount != 0 || lastError != nil {
		t.Errorf("queue metadata = version=%d retry=%d error=%v, want 2/0/nil",
			queuedVersion, retryCount, lastError)
	}
}

func TestApplyPullPage_SameNameSyncedMembershipTypeIsNotAutoRenamed(t *testing.T) {
	ctx := context.Background()
	db, uow, gymID := setupSidecarDB(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	localTypeID := uuid.New()
	cloudTypeID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO membership_types
		    (id, gym_id, version, created_at, updated_at, synced_at,
		     name, price, duration_days, enrollment_fee, maintenance_fee, active)
		VALUES (?, ?, 1, ?, ?, ?, 'Normal', 40000, 30, 0, 0, 1)`,
		localTypeID, gymID, now.UnixMilli(), now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatalf("seed canonical local type: %v", err)
	}
	change := syncpkg.PullChange{
		EntityType:      "membership_types",
		EntityID:        cloudTypeID.String(),
		Version:         1,
		Payload:         membershipTypePayload(cloudTypeID, gymID, "Normal", 450, now),
		ServerUpdatedAt: now.Add(time.Minute),
	}
	err := syncpkg.ApplyPullPage(ctx, uow, []syncpkg.PullChange{change}, nil)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("error = %v, want UNIQUE constraint (synced row must not be renamed)", err)
	}
	var name string
	_ = db.Get(&name, `SELECT name FROM membership_types WHERE id = ?`, localTypeID)
	if name != "Normal" {
		t.Errorf("synced type was mutated to %q", name)
	}
}

func membershipTypePayload(id, gymID uuid.UUID, name string, price float64, now time.Time) []byte {
	payload, _ := json.Marshal(map[string]any{
		"id": id.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(),
		"name": name, "price": price, "duration_days": 30, "duration_months": 1,
		"enrollment_fee": 0, "maintenance_fee": 0,
		"maintenance_frequency": nil, "active": true,
	})
	return payload
}
