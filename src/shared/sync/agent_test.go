//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// fakeCloud is the minimum HTTP surface a sidecar agent talks to.
// It is in-memory, deterministic, and deliberately tiny — we test the
// agent's behaviour, not the cloud's.
type fakeCloud struct {
	mu      sync.Mutex
	rows    map[string]map[string]any // gym|type|id -> snapshot
	pushes  []syncpkg.PushRequest
	failNxt atomic.Int32
	srv     *httptest.Server
	now     func() time.Time
}

func newFakeCloud(t *testing.T) *fakeCloud {
	t.Helper()
	c := &fakeCloud{
		rows: map[string]map[string]any{},
		now:  func() time.Time { return time.Now().UTC() },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync/push", c.push)
	mux.HandleFunc("/api/v1/sync/pull", c.pull)
	mux.HandleFunc("/api/v1/sync/full", c.full)
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeCloud) push(w http.ResponseWriter, r *http.Request) {
	if c.failNxt.Add(-1) >= 0 {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	var req syncpkg.PushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pushes = append(c.pushes, req)
	resp := syncpkg.PushResponse{ServerNow: c.now(), SchemaVersion: syncpkg.SchemaVersion}
	for _, it := range req.Batch {
		var pl map[string]any
		_ = json.Unmarshal(it.Payload, &pl)
		key := fmt.Sprintf("%s|%s|%s", pl["gym_id"], it.EntityType, it.EntityID)
		now := c.now()
		existing, exists := c.rows[key]
		if !exists {
			c.rows[key] = map[string]any{
				"version":           it.ClientVersion,
				"payload":           pl,
				"server_updated_at": now,
			}
			t := now
			resp.Results = append(resp.Results, syncpkg.PushItemResult{
				QueueID: it.QueueID, EntityID: it.EntityID,
				Status: syncpkg.StatusAccepted, ServerVersion: it.ClientVersion, ServerUpdatedAt: &t,
			})
			continue
		}
		// Existing — accept if newer, else conflict_server_wins.
		ev := existing["version"].(int)
		if ev < it.ClientVersion {
			c.rows[key] = map[string]any{
				"version":           it.ClientVersion,
				"payload":           pl,
				"server_updated_at": now,
			}
			t := now
			resp.Results = append(resp.Results, syncpkg.PushItemResult{
				QueueID: it.QueueID, EntityID: it.EntityID,
				Status: syncpkg.StatusAccepted, ServerVersion: it.ClientVersion, ServerUpdatedAt: &t,
			})
			continue
		}
		// Conflict — server wins.
		serverUpdatedAt := existing["server_updated_at"].(time.Time)
		serverPayload, _ := json.Marshal(existing["payload"])
		t := serverUpdatedAt
		resp.Results = append(resp.Results, syncpkg.PushItemResult{
			QueueID: it.QueueID, EntityID: it.EntityID,
			Status: syncpkg.StatusConflictServerWins, ServerVersion: ev,
			ServerUpdatedAt: &t, ServerPayload: serverPayload,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (c *fakeCloud) pull(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	since := r.URL.Query().Get("since")
	var sinceT time.Time
	if since != "" {
		sinceT, _ = time.Parse(time.RFC3339Nano, since)
	}
	resp := syncpkg.PullResponse{ServerNow: c.now(), SchemaVersion: syncpkg.SchemaVersion}
	for key, row := range c.rows {
		updatedAt := row["server_updated_at"].(time.Time)
		if !updatedAt.After(sinceT) {
			continue
		}
		// key = gym|type|id
		parts := splitKey(key)
		pl, _ := json.Marshal(row["payload"])
		resp.Changes = append(resp.Changes, syncpkg.PullChange{
			EntityType: parts[1], EntityID: parts[2],
			Version: row["version"].(int),
			Payload: pl, ServerUpdatedAt: updatedAt,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (c *fakeCloud) full(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp := syncpkg.FullSyncResponse{ServerNow: c.now(), SchemaVersion: syncpkg.SchemaVersion}
	// Trivial: emit every row in one page (tests don't exercise pagination here).
	for key, row := range c.rows {
		parts := splitKey(key)
		pl, _ := json.Marshal(row["payload"])
		resp.Changes = append(resp.Changes, syncpkg.PullChange{
			EntityType: parts[1], EntityID: parts[2],
			Version: row["version"].(int),
			Payload: pl, ServerUpdatedAt: row["server_updated_at"].(time.Time),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func splitKey(k string) []string {
	parts := []string{"", "", ""}
	idx := 0
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			idx++
			continue
		}
		parts[idx] += string(k[i])
	}
	return parts
}

// ── Test scaffolding — sqlite + uow + agent ────────────────────────────

func setupSidecarDB(t *testing.T) (*sqlx.DB, sharedDomain.UnitOfWork, uuid.UUID) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	dsn := dbPath + "?_foreign_keys=on"
	db, err := sqlx.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	// Aplicamos TODAS las migraciones sqlite en orden — no un subset
	// cherry-picked. El schema del test tiene que matchear producción:
	// el sync agent puede pull-ear filas con cualquier columna que una
	// migración haya agregado (ej. members.pin_plain de la 009), y un
	// subset desactualizado rompe en silencio cada vez que se agrega una.
	// os.ReadDir devuelve las entries ordenadas por nombre, y los archivos
	// están zero-padded (001_..016_) → orden léxico == orden numérico.
	migDir := "../../../db_migrations/sqlite"
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		m := filepath.Join(migDir, e.Name())
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}
	uow := sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue())

	// Seed a gym row so FK constraints don't bite us when we insert
	// dependents during testing.
	gymID := uuid.New()
	now := time.Now().UTC().UnixMilli()
	_, err = db.Exec(`INSERT INTO gyms (id, gym_id, version, created_at, updated_at) VALUES (?, ?, 1, ?, ?)`,
		gymID, gymID, now, now)
	if err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	// Seed an operator user so members.created_by FK is satisfied for tests
	// that insert members directly.
	userID := uuid.New()
	_, err = db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at,
		                   email, password_hash, full_name, role, active)
		VALUES (?, ?, 1, ?, ?, 'op@gym.com', 'x', 'Op', 'operator', 1)`,
		userID, gymID, now, now)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Setenv("SEED_USER_ID", userID.String())
	return db, uow, gymID
}

func newAgent(t *testing.T, cloud *fakeCloud, db *sqlx.DB, uow sharedDomain.UnitOfWork, token string) *syncpkg.Agent {
	t.Helper()
	a := syncpkg.NewAgent(syncpkg.AgentConfig{
		BaseURL:          cloud.srv.URL,
		Interval:         24 * time.Hour, // disabled — tests drive RunOnce
		BatchSize:        50,
		PullPageSize:     100,
		FullSyncPageSize: 200,
		HTTPClient:       cloud.srv.Client(),
	}, db, uow)
	a.SetToken(token)
	if err := a.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return a
}

func enqueueMember(t *testing.T, db *sqlx.DB, gymID uuid.UUID, version int) uuid.UUID {
	t.Helper()
	memberID := uuid.New()
	now := time.Now().UTC().UnixMilli()
	userID := os.Getenv("SEED_USER_ID")
	folio := "M-" + memberID.String()[:8]
	_, err := db.Exec(`
		INSERT INTO members (id, gym_id, version, created_at, updated_at,
		                     folio, full_name, phone, status, created_by)
		VALUES (?, ?, ?, ?, ?, ?, 'Test Member', '5555555', 'active', ?)`,
		memberID, gymID, version, now, now, folio, userID)
	if err != nil {
		t.Fatalf("insert member: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"id":         memberID.String(),
		"gym_id":     gymID.String(),
		"version":    version,
		"folio":      folio,
		"full_name":  "Test Member",
		"phone":      "5555555",
		"status":     "active",
		"updated_at": now,
		"created_by": userID,
	})
	enqueueID := uuid.New()
	_, err = db.Exec(`
		INSERT INTO sync_queue (id, entity_type, entity_id, operation, payload, client_version, enqueued_at)
		VALUES (?, 'members', ?, 'upsert', ?, ?, ?)`,
		enqueueID, memberID, string(payload), version, now)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return memberID
}

// ── Tests ──────────────────────────────────────────────────────────────

// TestAgentPush_FlushesQueueAndMarksSynced verifies the happy path:
// pending rows go to the cloud and are marked synced, the entity table's
// synced_at gets stamped, and a second push is a no-op.
func TestAgentPush_FlushesQueueAndMarksSynced(t *testing.T) {
	cloud := newFakeCloud(t)
	db, uow, gymID := setupSidecarDB(t)
	memberID := enqueueMember(t, db, gymID, 1)

	mustMarkInitialSyncDone(t, db, uow)
	a := newAgent(t, cloud, db, uow, "fake-token")

	a.RunOnce(context.Background())

	// Queue row marked synced.
	var syncedAt *int64
	if err := db.Get(&syncedAt, `SELECT synced_at FROM sync_queue WHERE entity_id = ?`, memberID); err != nil {
		t.Fatalf("query queue: %v", err)
	}
	if syncedAt == nil {
		t.Fatalf("sync_queue.synced_at not set")
	}

	// Member's synced_at also stamped.
	var memSynced *int64
	if err := db.Get(&memSynced, `SELECT synced_at FROM members WHERE id = ?`, memberID); err != nil {
		t.Fatalf("query member: %v", err)
	}
	if memSynced == nil {
		t.Errorf("members.synced_at not stamped")
	}

	// Cloud received exactly one push.
	cloud.mu.Lock()
	pushCount := len(cloud.pushes)
	cloud.mu.Unlock()
	if pushCount != 1 {
		t.Errorf("cloud got %d pushes, want 1", pushCount)
	}

	// Idempotent re-run = no-op.
	a.RunOnce(context.Background())
	cloud.mu.Lock()
	pushCount = len(cloud.pushes)
	cloud.mu.Unlock()
	if pushCount != 1 {
		t.Errorf("second RunOnce caused %d pushes, want still 1", pushCount)
	}
}

// TestAgentBackoff verifies a 5xx puts the agent into a backoff window so
// repeated RunOnce calls don't hammer the cloud.
func TestAgentBackoff(t *testing.T) {
	cloud := newFakeCloud(t)
	cloud.failNxt.Store(2) // first two pushes 500
	db, uow, gymID := setupSidecarDB(t)
	enqueueMember(t, db, gymID, 1)
	mustMarkInitialSyncDone(t, db, uow)
	a := newAgent(t, cloud, db, uow, "fake-token")

	a.RunOnce(context.Background())
	snap1 := a.Snapshot()
	if snap1.ConsecutiveFailures != 1 {
		t.Errorf("failures=%d, want 1", snap1.ConsecutiveFailures)
	}
	if snap1.NextRetryAt.IsZero() {
		t.Errorf("backoff window not set")
	}

	// Inside backoff window → RunOnce should short-circuit (no new push).
	cloud.mu.Lock()
	before := len(cloud.pushes)
	cloud.mu.Unlock()
	a.RunOnce(context.Background())
	cloud.mu.Lock()
	after := len(cloud.pushes)
	cloud.mu.Unlock()
	if after != before {
		t.Errorf("push attempted inside backoff window: %d -> %d", before, after)
	}
}

// TestAgentPull_AppliesServerCanonicalState — when the server has data the
// sidecar doesn't, pull lands it in the local domain table with version
// equal to the server's.
func TestAgentPull_AppliesServerCanonicalState(t *testing.T) {
	cloud := newFakeCloud(t)
	db, uow, gymID := setupSidecarDB(t)
	mustMarkInitialSyncDone(t, db, uow)
	a := newAgent(t, cloud, db, uow, "fake-token")

	// Seed cloud with a member the sidecar has never seen.
	cloudMemberID := uuid.New()
	userID := os.Getenv("SEED_USER_ID")
	cloud.rows["serverkey"] = nil // placeholder so we don't seed via push
	cloud.mu.Lock()
	nowMs := time.Now().UnixMilli()
	cloud.rows[fmt.Sprintf("%s|members|%s", gymID.String(), cloudMemberID.String())] = map[string]any{
		"version": 7,
		"payload": map[string]any{
			"id":              cloudMemberID.String(),
			"gym_id":          gymID.String(),
			"version":         7,
			"folio":           "M-CLOUD",
			"full_name":       "Cloud Member",
			"phone":           "999",
			"status":          "active",
			"enrollment_paid": false,
			"created_at":      nowMs,
			"updated_at":      nowMs,
			"created_by":      userID,
		},
		"server_updated_at": time.Now().UTC(),
	}
	delete(cloud.rows, "serverkey")
	cloud.mu.Unlock()

	a.RunOnce(context.Background())
	if snap := a.Snapshot(); snap.LastError != "" {
		t.Logf("agent state after RunOnce: %+v", snap)
	}

	var name string
	if err := db.Get(&name, `SELECT full_name FROM members WHERE id = ?`, cloudMemberID); err != nil {
		t.Fatalf("expected pulled member to exist: %v (snap=%+v)", err, a.Snapshot())
	}
	if name != "Cloud Member" {
		t.Errorf("name = %q, want Cloud Member", name)
	}
	var version int
	_ = db.Get(&version, `SELECT version FROM members WHERE id = ?`, cloudMemberID)
	if version != 7 {
		t.Errorf("version = %d, want 7", version)
	}
}

// TestAtomicidad_SyncQueueAndMutationCommitTogether is the headline ADR
// guarantee (§3.10): a domain mutation and its sync_queue row must commit
// or rollback together. We force a failure mid-Command and assert NEITHER
// side persisted.
func TestAtomicidad_SyncQueueAndMutationCommitTogether(t *testing.T) {
	db, uow, gymID := setupSidecarDB(t)
	userID := os.Getenv("SEED_USER_ID")
	want := fmt.Errorf("forced rollback")

	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		stx := tx.(*sharedDomain.SqlxTransaction)
		now := time.Now().UTC().UnixMilli()
		mid := uuid.New()
		if _, err := stx.Exec(context.Background(), `
			INSERT INTO members (id, gym_id, version, created_at, updated_at,
			                     folio, full_name, phone, status, created_by)
			VALUES (?, ?, 1, ?, ?, 'M-ATOMIC', 'Atomic', '0', 'active', ?)`,
			mid, gymID, now, now, userID); err != nil {
			return err
		}
		if err := stx.EnqueueSync(context.Background(), "members", mid.String(), "upsert",
			[]byte(`{"id":"`+mid.String()+`"}`), 1); err != nil {
			return err
		}
		return want
	})
	if err == nil {
		t.Fatalf("expected forced error to surface")
	}

	var memberCount, queueCount int
	_ = db.Get(&memberCount, `SELECT COUNT(*) FROM members WHERE folio = 'M-ATOMIC'`)
	_ = db.Get(&queueCount, `SELECT COUNT(*) FROM sync_queue WHERE entity_type = 'members'`)
	if memberCount != 0 {
		t.Errorf("member persisted despite rollback: %d", memberCount)
	}
	if queueCount != 0 {
		t.Errorf("sync_queue persisted despite rollback: %d", queueCount)
	}
}

// TestAtomicidad_BothSidesCommit — the positive case. Same path, no error,
// both sides land.
func TestAtomicidad_BothSidesCommit(t *testing.T) {
	db, uow, gymID := setupSidecarDB(t)
	userID := os.Getenv("SEED_USER_ID")
	mid := uuid.New()
	now := time.Now().UTC().UnixMilli()
	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		stx := tx.(*sharedDomain.SqlxTransaction)
		if _, err := stx.Exec(context.Background(), `
			INSERT INTO members (id, gym_id, version, created_at, updated_at,
			                     folio, full_name, phone, status, created_by)
			VALUES (?, ?, 1, ?, ?, 'M-OK', 'OK', '0', 'active', ?)`,
			mid, gymID, now, now, userID); err != nil {
			return err
		}
		return stx.EnqueueSync(context.Background(), "members", mid.String(), "upsert",
			[]byte(`{"id":"`+mid.String()+`"}`), 1)
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	var n int
	_ = db.Get(&n, `SELECT COUNT(*) FROM sync_queue WHERE entity_id = ?`, mid)
	if n != 1 {
		t.Errorf("queue row missing: %d", n)
	}
	_ = db.Get(&n, `SELECT COUNT(*) FROM members WHERE id = ?`, mid)
	if n != 1 {
		t.Errorf("member missing: %d", n)
	}
}

// TestSyncQueueCoalescing — repeated mutations to the same entity within
// a push interval should collapse into one queue row (ADR-001 §3.2).
func TestSyncQueueCoalescing(t *testing.T) {
	db, _, gymID := setupSidecarDB(t)
	userID := os.Getenv("SEED_USER_ID")
	mid := uuid.New()
	now := time.Now().UTC().UnixMilli()
	_, err := db.Exec(`
		INSERT INTO members (id, gym_id, version, created_at, updated_at,
		                     folio, full_name, phone, status, created_by)
		VALUES (?, ?, 1, ?, ?, 'M-001', 'X', '0', 'active', ?)`,
		mid, gymID, now, now, userID)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	q := syncpkg.NewSqliteQueue()
	uow := sharedDomain.NewSQLiteUnitOfWork(db, q)
	for v := 1; v <= 5; v++ {
		err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
			return tx.(*sharedDomain.SqlxTransaction).EnqueueSync(context.Background(),
				"members", mid.String(), "upsert",
				[]byte(fmt.Sprintf(`{"v":%d}`, v)), v)
		})
		if err != nil {
			t.Fatalf("enqueue v=%d: %v", v, err)
		}
	}
	var n int
	_ = db.Get(&n, `SELECT COUNT(*) FROM sync_queue WHERE entity_id = ? AND synced_at IS NULL`, mid)
	if n != 1 {
		t.Errorf("expected coalesced into 1 row, have %d", n)
	}
	var clientVersion int
	_ = db.Get(&clientVersion, `SELECT client_version FROM sync_queue WHERE entity_id = ?`, mid)
	if clientVersion != 5 {
		t.Errorf("coalesced version = %d, want 5", clientVersion)
	}
}

// mustMarkInitialSyncDone — the agent skips full-sync only after this state
// is set. Call BEFORE newAgent() so the agent's bootstrap picks it up.
func mustMarkInitialSyncDone(t *testing.T, db *sqlx.DB, uow sharedDomain.UnitOfWork) {
	t.Helper()
	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.SetInitialSyncCompletedAt(context.Background(), tx, time.Now())
	})
	if err != nil {
		t.Fatalf("mark initial sync: %v", err)
	}
}
