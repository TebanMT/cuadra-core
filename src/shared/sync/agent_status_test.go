//go:build sidecar

package sync

import (
	"testing"
	"time"
)

// TestStatusThresholds covers UC-044 / DA-44.x state transitions. Pure
// function — no agent or DB needed.
//
// Semántica corregida: el clasificador prioriza `ConsecutiveFailures` y
// `LastError`. Un gap viejo SIN fallas no marca offline — el agente puede
// estar idle o silenciado por falta de token, y pintar amarillo asusta al
// dueño sin causa real.
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
		{"30s ago, sin fallas → online",
			AgentSnapshot{LastSyncedAt: now.Add(-30 * time.Second), InitialSyncCompletedAt: now.Add(-time.Hour)},
			StateOnline,
		},
		{
			// REGRESSION: antes esto salía como `offline_medium` y el FE pintaba
			// "Sin internet, todo guardado en esta laptop." aunque el agente
			// estaba sano (sin error, sin falla). El bug que el dueño reportó.
			name: "10 min ago, sin fallas → online (no asustar)",
			snap: AgentSnapshot{
				LastSyncedAt:           now.Add(-10 * time.Minute),
				InitialSyncCompletedAt: now.Add(-time.Hour),
			},
			wantState: StateOnline,
		},
		{"10 min ago + 1 falla reciente → offline_short (transitorio)",
			AgentSnapshot{
				LastSyncedAt:           now.Add(-10 * time.Minute),
				InitialSyncCompletedAt: now.Add(-time.Hour),
				ConsecutiveFailures:    1,
				LastError:              "push: connection refused",
			},
			StateOfflineMedium,
		},
		{"2min ago + 1 falla → offline_short",
			AgentSnapshot{
				LastSyncedAt:           now.Add(-2 * time.Minute),
				InitialSyncCompletedAt: now.Add(-time.Hour),
				ConsecutiveFailures:    1,
				LastError:              "push: connection refused",
			},
			StateOfflineShort,
		},
		{"5+ fallas → offline_medium",
			AgentSnapshot{
				LastSyncedAt:           now.Add(-15 * time.Minute),
				InitialSyncCompletedAt: now.Add(-time.Hour),
				ConsecutiveFailures:    6,
				LastError:              "pull: 502",
			},
			StateOfflineMedium,
		},
		{"30 h ago, sin fallas → long (gap >24h sí escala aunque no haya error)",
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
		{
			// 401 del cloud: la credencial está muerta. La UI necesita CTA
			// de re-login, no "Sin internet" — incluso si la última sync
			// fue hace minutos.
			name: "AuthInvalid pisa la clasificación por gap",
			snap: AgentSnapshot{
				LastSyncedAt:           now.Add(-2 * time.Minute),
				InitialSyncCompletedAt: now.Add(-time.Hour),
				AuthInvalid:            true,
				LastError:              "push: 401 unauthorized",
			},
			wantState: StateAuthInvalid,
		},
		{
			// Sidecar recién canjeado todavía sin token: spinner inicial,
			// no error.
			name:      "WaitingForAuth sin sync previo → initial_syncing",
			snap:      AgentSnapshot{WaitingForAuth: true},
			wantState: StateInitialSyncing,
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

// TestStatusResponse_AuthInvalidFlag — el flag auth_invalid del wire
// surface también cuando WaitingForAuth está activo, no sólo AuthInvalid.
// Desde el operador es el mismo problema: hace falta autenticar.
func TestStatusResponse_AuthInvalidFlag(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		snap AgentSnapshot
		want bool
	}{
		{"healthy → flag false", AgentSnapshot{LastSyncedAt: now.Add(-time.Minute)}, false},
		{"AuthInvalid → flag true", AgentSnapshot{LastSyncedAt: now.Add(-time.Minute), AuthInvalid: true}, true},
		{"WaitingForAuth → flag true", AgentSnapshot{WaitingForAuth: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildStatusResponse(tc.snap, now)
			if got.AuthInvalid != tc.want {
				t.Errorf("auth_invalid = %v, want %v", got.AuthInvalid, tc.want)
			}
		})
	}
}

// TestIsAuthError reconoce ErrUnauthorized sentinel y mensajes que vienen
// con "401" / "unauthorized" (upload/download a R2 envuelven con fmt.Errorf).
func TestIsAuthError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil → false", nil, false},
		{"sentinel", ErrUnauthorized, true},
		{"wrapped 401 string", &simpleErr{"upload 401: bad token"}, true},
		{"unauthorized string", &simpleErr{"r2 upload: unauthorized"}, true},
		{"5xx not auth", &simpleErr{"push 5xx: 503"}, false},
		{"network not auth", &simpleErr{"dial tcp: connection refused"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAuthError(tc.err); got != tc.want {
				t.Errorf("isAuthError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

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
