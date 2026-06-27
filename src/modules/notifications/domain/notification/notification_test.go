package notification

import (
	"testing"
	"time"
)

func newPending(t *testing.T) *Notification {
	t.Helper()
	n := &Notification{
		Status:    StatusPending,
		Version:   1,
		CreatedAt: time.Unix(0, 0).UTC(),
	}
	return n
}

func TestMarkHeld_FromPending(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	n := newPending(t)
	n.MarkHeld("retenido por demora: 240h", now)

	if n.Status != StatusHeld {
		t.Fatalf("status = %q, want held", n.Status)
	}
	if n.ErrorMessage == nil || *n.ErrorMessage == "" {
		t.Errorf("la razón del held debería quedar en ErrorMessage")
	}
	if n.Version != 2 {
		t.Errorf("version = %d, want 2 (bump)", n.Version)
	}
	if !n.UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %v, want %v", n.UpdatedAt, now)
	}
}

func TestMarkHeld_NoopWhenNotPending(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	for _, st := range []string{StatusSent, StatusFailed, StatusCancelled, StatusHeld} {
		n := newPending(t)
		n.Status = st
		n.Version = 5
		n.MarkHeld("x", now)
		if n.Status != st {
			t.Errorf("desde %q: status cambió a %q (debería ser no-op)", st, n.Status)
		}
		if n.Version != 5 {
			t.Errorf("desde %q: version cambió (debería ser no-op)", st)
		}
	}
}
