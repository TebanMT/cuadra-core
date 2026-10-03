//go:build sidecar

package sync

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProgressDoesNotReplaceSyncErrors(t *testing.T) {
	now := time.Now().UTC()
	for _, state := range []AgentSnapshot{
		{SyncInProgress: true, SchemaUpgradeRequired: true},
		{SyncInProgress: true, AuthInvalid: true},
		{SyncInProgress: true, StuckPushCount: 1},
	} {
		busy := buildStatusResponse(state, now)
		state.SyncInProgress = false
		idle := buildStatusResponse(state, now)
		if !busy.SyncInProgress || idle.SyncInProgress || busy.State != idle.State {
			t.Fatalf("activity changed the error classification: busy=%+v idle=%+v", busy, idle)
		}
	}
}

func TestProgressStopsWhenAttemptCannotRun(t *testing.T) {
	for _, mode := range []string{"no tenant", "no token", "backoff", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			a := NewAgent(AgentConfig{BaseURL: "http://unused.invalid"}, nil, nil)
			if mode == "disabled" {
				a.cfg.BaseURL = ""
				a.TriggerNow()
			} else {
				if mode != "no tenant" {
					a.activeGymID = uuid.New()
				}
				if mode == "backoff" {
					a.token = "test-only"
					a.state.NextRetryAt = time.Now().Add(time.Hour)
				}
				a.TriggerNow()
				<-a.trigger // same dequeue as Run, without touching storage or networking
				a.RunOnce(context.Background())
			}
			if a.Snapshot().SyncInProgress {
				t.Fatal("an inactive attempt must not leave the animation running")
			}
		})
	}
}
