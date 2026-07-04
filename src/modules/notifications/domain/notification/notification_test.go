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

func TestReconcileDeliveryFailure_FromSent(t *testing.T) {
	sentAt := time.Unix(500, 0).UTC()
	now := time.Unix(1000, 0).UTC()
	n := newPending(t)
	n.Status = StatusSent
	n.SentAt = &sentAt
	n.Version = 2

	if !n.ReconcileDeliveryFailure("twilio: undelivered (error 63024)", now) {
		t.Fatal("desde sent debería reconciliar (return true)")
	}
	if n.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", n.Status)
	}
	if n.SentAt != nil {
		t.Errorf("SentAt debería limpiarse (no contar como entregada Y fallida en stats)")
	}
	if n.FailedAt == nil || !n.FailedAt.Equal(now) {
		t.Errorf("FailedAt = %v, want %v", n.FailedAt, now)
	}
	if n.ErrorMessage == nil || *n.ErrorMessage != "twilio: undelivered (error 63024)" {
		t.Errorf("ErrorMessage = %v, want la razón del provider", n.ErrorMessage)
	}
	if n.Version != 3 {
		t.Errorf("version = %d, want 3 (bump)", n.Version)
	}
}

func TestReconcileDeliveryFailure_FromPending(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	n := newPending(t)
	if !n.ReconcileDeliveryFailure("twilio: failed", now) {
		t.Fatal("desde pending debería reconciliar (return true)")
	}
	if n.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", n.Status)
	}
}

func TestReconcileDeliveryFailure_NoopFromTerminalStates(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	for _, st := range []string{StatusFailed, StatusCancelled, StatusHeld} {
		n := newPending(t)
		n.Status = st
		n.Version = 5
		if n.ReconcileDeliveryFailure("x", now) {
			t.Errorf("desde %q: debería ser no-op (return false)", st)
		}
		if n.Status != st {
			t.Errorf("desde %q: status cambió a %q", st, n.Status)
		}
		if n.Version != 5 {
			t.Errorf("desde %q: version cambió (callback duplicado debe ser idempotente)", st)
		}
	}
}
