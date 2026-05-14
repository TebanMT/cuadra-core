//go:build sidecar

package sync

import (
	"testing"
	"time"
)

// TestStatusThresholds covers UC-044 / DA-44.x state transitions. Pure
// function — no agent or DB needed.
func TestStatusThresholds(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		snap      AgentSnapshot
		wantState string
	}{
		{"never synced, no error → initial_syncing (recién pareado)",
			AgentSnapshot{},
			StateInitialSyncing,
		},
		{"never synced + error → critical (intento fallido)",
			AgentSnapshot{LastError: "push: 401 unauthorized"},
			StateOfflineCritical,
		},
		{"30s ago → online",
			AgentSnapshot{LastSyncedAt: now.Add(-30 * time.Second), InitialSyncCompletedAt: now.Add(-time.Hour)},
			StateOnline,
		},
		{"10 min ago → medium",
			AgentSnapshot{LastSyncedAt: now.Add(-10 * time.Minute), InitialSyncCompletedAt: now.Add(-time.Hour)},
			StateOfflineMedium,
		},
		{"30 h ago → long",
			AgentSnapshot{LastSyncedAt: now.Add(-30 * time.Hour), InitialSyncCompletedAt: now.Add(-time.Hour)},
			StateOfflineLong,
		},
		{"10 d ago → critical",
			AgentSnapshot{LastSyncedAt: now.Add(-10 * 24 * time.Hour), InitialSyncCompletedAt: now.Add(-time.Hour)},
			StateOfflineCritical,
		},
		{"initial syncing label",
			AgentSnapshot{State: StateInitialSyncing},
			StateInitialSyncing,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildStatusResponse(tc.snap, now)
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q", got.State, tc.wantState)
			}
		})
	}
}

func TestBackoffSequence(t *testing.T) {
	want := []time.Duration{
		0,
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		60 * time.Second,
		5 * time.Minute,
		5 * time.Minute,
	}
	for i, w := range want {
		if got := backoff(i); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i, got, w)
		}
	}
}
