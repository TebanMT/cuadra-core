//go:build sidecar && chaos

// Chaos tests run only when explicitly built with `-tags chaos` (so CI's
// default `-tags "server sidecar"` skips them — ADR-001 §6.3 marks these
// as locally-runnable but excluded from the standard pipeline).
//
// Run with:
//   go test -tags "sidecar chaos" -run TestChaos ./src/shared/sync/
//
// The latency test starts a fake cloud that sleeps before responding; the
// agent should still flush its queue without dropping items.

package sync_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

func TestChaos_PushUnderLatency(t *testing.T) {
	cloud := newFakeCloud(t)
	// Wrap the fake cloud handler with a sleep.
	original := cloud.srv.Config.Handler
	cloud.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		original.ServeHTTP(w, r)
	})

	db, uow, gymID := setupSidecarDB(t)
	for i := 0; i < 5; i++ {
		enqueueMember(t, db, gymID, 1)
	}
	mustMarkInitialSyncDone(t, db, uow)
	a := newAgent(t, cloud, db, uow, "fake-token")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.RunOnce(ctx)

	// Verify all queue rows synced.
	var pending int
	if err := db.Get(&pending, `SELECT COUNT(*) FROM sync_queue WHERE synced_at IS NULL`); err != nil {
		t.Fatalf("query: %v", err)
	}
	if pending != 0 {
		t.Errorf("after sync under 200ms latency, %d rows still pending", pending)
	}
	_ = httptest.NewServer
	_ = syncpkg.SchemaVersion
}
