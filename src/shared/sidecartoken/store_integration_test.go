//go:build server && integration

package sidecartoken_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/cuadra/cuadra-core/src/shared/sidecartoken"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/cuadra?sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Skipf("integration test skipped — Postgres unreachable: %v", err)
	}
	return db
}

// seedGym creates a minimal gym + user so the FK in sidecar_credentials
// holds. Returns (gymID, userID) and registers cleanup.
func seedGym(t *testing.T, db *gorm.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	gymID := uuid.New()
	userID := uuid.New()
	if err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone)
		VALUES (?, ?, 1, NOW(), NOW(), 'AutoRevoke Gym', 'MX', 'America/Mexico_City')`,
		gymID, gymID).Error; err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role, active)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 'unused', 'Owner', 'owner', TRUE)`,
		userID, gymID, "rev-"+gymID.String()+"@test.local").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Exec(`DELETE FROM sidecar_credentials WHERE gym_id = ?`, gymID).Error
		_ = db.Exec(`DELETE FROM users WHERE gym_id = ?`, gymID).Error
		_ = db.Exec(`DELETE FROM gyms WHERE id = ?`, gymID).Error
	})
	return gymID, userID
}

func TestStore_RoundTripInsertLookupRevoke(t *testing.T) {
	db := openTestDB(t)
	gymID, userID := seedGym(t, db)
	store := sidecartoken.NewPostgresStore(db)
	ctx := context.Background()

	tok, hash, err := sidecartoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	clientID := uuid.New()
	cred, err := store.Insert(ctx, gymID, clientID, userID, hash, "laptop")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if cred.ID == uuid.Nil {
		t.Errorf("Insert returned nil ID")
	}

	// Lookup by hash works.
	got, err := store.LookupActiveByHash(ctx, sidecartoken.Hash(tok))
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.GymID != gymID || got.ClientID != clientID || got.UserID != userID {
		t.Errorf("lookup mismatch: %+v", got)
	}

	// Revoke + lookup misses.
	if err := store.RevokeActive(ctx, gymID, clientID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := store.LookupActiveByHash(ctx, sidecartoken.Hash(tok)); err != sidecartoken.ErrNotFound {
		t.Errorf("revoked credential still found: err=%v", err)
	}
}

func TestStore_RevokeIdleSweepsOldRows(t *testing.T) {
	db := openTestDB(t)
	gymID, userID := seedGym(t, db)
	store := sidecartoken.NewPostgresStore(db)
	ctx := context.Background()

	// Insert two credentials and back-date one of them past the threshold.
	freshID := uuid.New()
	staleID := uuid.New()
	if _, err := store.Insert(ctx, gymID, freshID, userID, sidecartoken.Hash("fresh"), "fresh"); err != nil {
		t.Fatalf("insert fresh: %v", err)
	}
	if _, err := store.Insert(ctx, gymID, staleID, userID, sidecartoken.Hash("stale"), "stale"); err != nil {
		t.Fatalf("insert stale: %v", err)
	}
	if err := db.Exec(`UPDATE sidecar_credentials SET last_seen_at = NOW() - INTERVAL '40 days' WHERE client_id = ?`, staleID).Error; err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := store.RevokeIdle(ctx, time.Now().UTC().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("revoke idle: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 revoked row, got %d", n)
	}
	// Fresh credential survives.
	if _, err := store.LookupActiveByHash(ctx, sidecartoken.Hash("fresh")); err != nil {
		t.Errorf("fresh credential revoked unexpectedly: %v", err)
	}
}

func TestStore_FindActiveAndUniqueConstraint(t *testing.T) {
	db := openTestDB(t)
	gymID, userID := seedGym(t, db)
	store := sidecartoken.NewPostgresStore(db)
	ctx := context.Background()

	clientID := uuid.New()
	if _, err := store.Insert(ctx, gymID, clientID, userID, sidecartoken.Hash("a"), "dev"); err != nil {
		t.Fatal(err)
	}

	// FindActive returns the row.
	got, err := store.FindActive(ctx, gymID, clientID)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.ClientID != clientID {
		t.Errorf("find mismatch")
	}

	// Re-insert without revoking should be blocked by the unique partial
	// index — caller must revoke first.
	if _, err := store.Insert(ctx, gymID, clientID, userID, sidecartoken.Hash("b"), "dev"); err == nil {
		t.Errorf("expected unique constraint violation on duplicate active credential")
	}

	// Revoke + insert again works.
	if err := store.RevokeActive(ctx, gymID, clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Insert(ctx, gymID, clientID, userID, sidecartoken.Hash("c"), "dev"); err != nil {
		t.Errorf("re-insert after revoke failed: %v", err)
	}
}

// TestStore_TouchLastSeenIsLazy — sanity check that TouchLastSeen runs
// without the goroutine ever waiting.
func TestStore_TouchLastSeen(t *testing.T) {
	db := openTestDB(t)
	gymID, userID := seedGym(t, db)
	store := sidecartoken.NewPostgresStore(db)
	ctx := context.Background()

	cred, err := store.Insert(ctx, gymID, uuid.New(), userID, sidecartoken.Hash("x"), "")
	if err != nil {
		t.Fatal(err)
	}
	before := cred.LastSeenAt
	time.Sleep(20 * time.Millisecond)
	if err := store.TouchLastSeen(ctx, cred.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, err := store.LookupActiveByHash(ctx, sidecartoken.Hash("x"))
	if err != nil {
		t.Fatalf("re-lookup: %v", err)
	}
	if !got.LastSeenAt.After(before) {
		t.Errorf("last_seen_at not updated: before=%s after=%s", before, got.LastSeenAt)
	}
}
