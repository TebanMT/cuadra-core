//go:build sidecar

package sync_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

type progressTransport func(*http.Request) (*http.Response, error)

func (fn progressTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestAgentProgressCoversQueuedAndRunningCycles(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			cloud := newFakeCloud(t)
			db, uow, _ := setupSidecarDB(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var pulls atomic.Int32
			base := cloud.srv.Client().Transport
			client := &http.Client{Transport: progressTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/v1/sync/pull" && pulls.Add(1) == 1 {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					if outcome == "failure" {
						return nil, errors.New("simulated connection failure")
					}
				}
				return base.RoundTrip(r)
			})}
			a := syncpkg.NewAgent(syncpkg.AgentConfig{
				BaseURL: cloud.srv.URL, Interval: time.Hour, HTTPClient: client,
				TokenProvider: func() string { return "test-only" },
			}, db, uow)
			a.TriggerNow()
			if !a.Snapshot().SyncInProgress {
				t.Fatal("accepted request must remain busy until the loop handles it")
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); a.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("agent did not stop")
				}
			})
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("agent did not start pulling")
			}
			if !a.Snapshot().SyncInProgress {
				t.Fatal("activity stopped while the cloud request was still running")
			}
			// Repeated clicks remain coalesced by the existing trigger channel.
			a.TriggerNow()
			a.TriggerNow()
			if outcome == "cancel" {
				cancel()
			} else {
				close(release)
			}
			deadline := time.Now().Add(5 * time.Second)
			for a.Snapshot().SyncInProgress && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			snap := a.Snapshot()
			if snap.SyncInProgress {
				t.Fatal("activity remained stuck after the cycle ended")
			}
			if outcome == "success" && (snap.LastSyncedAt.IsZero() || pulls.Load() != 2) {
				t.Fatalf("expected initial and queued cycles, got pulls=%d lastSync=%v", pulls.Load(), snap.LastSyncedAt)
			}
			if outcome == "failure" && snap.ConsecutiveFailures == 0 {
				t.Fatal("failed attempt must keep its error state")
			}
		})
	}
}
