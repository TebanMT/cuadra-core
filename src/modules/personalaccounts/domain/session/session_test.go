package session

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSessionInactivityWindowAndRevocationAreExact(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.FixedZone("fixture", -6*3600))
	s, err := NewSession(uuid.New(), uuid.New(), uuid.New(), [32]byte{1}, uuid.New(), [32]byte{2}, now)
	if err != nil || s.Generation != 1 || s.ExpiresAt.Sub(s.CreatedAt) != 90*24*time.Hour || !s.ActiveAt(now) || s.CreatedAt.Location() != time.UTC {
		t.Fatalf("wrong initial session: %v", err)
	}
	if s.ActiveAt(now.Add(-time.Nanosecond)) || !s.ActiveAt(s.ExpiresAt.Add(-time.Nanosecond)) || s.ActiveAt(s.ExpiresAt) {
		t.Fatal("wrong session boundary")
	}
	if err = s.Revoke(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	first := *s.RevokedAt
	if err = s.Revoke(now.Add(2 * time.Second)); err != nil || *s.RevokedAt != first || s.ActiveAt(now.Add(3*time.Second)) {
		t.Fatal("revocation changed or session reactivated")
	}
}

func TestSessionRejectsMissingOrReusedSecrets(t *testing.T) {
	for _, pair := range [][2][32]byte{{{}, {2}}, {{1}, {}}, {{1}, {1}}} {
		if _, err := NewSession(uuid.New(), uuid.New(), uuid.New(), pair[0], uuid.New(), pair[1], time.Now()); err == nil {
			t.Fatal("missing or reused secret verifier accepted")
		}
	}
	if _, err := NewSession(uuid.Nil, uuid.New(), uuid.New(), [32]byte{1}, uuid.New(), [32]byte{2}, time.Now()); err == nil {
		t.Fatal("missing owner accepted")
	}
	if _, err := NewSession(uuid.New(), uuid.New(), uuid.New(), [32]byte{1}, uuid.New(), [32]byte{2}, time.Time{}); err == nil {
		t.Fatal("missing server time accepted")
	}
}
