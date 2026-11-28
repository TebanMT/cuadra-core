package notification

import (
	"testing"
	"time"
)

func sentNotification(t *testing.T) *Notification {
	t.Helper()
	n := newPending(t)
	if err := n.MarkSent("SM123", time.Unix(2000, 0).UTC()); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	return n
}

func TestMarkDelivered_StampsOnceAndBumpsVersion(t *testing.T) {
	n := sentNotification(t)
	v := n.Version
	at := time.Unix(2010, 0).UTC()
	if !n.MarkDelivered(at) {
		t.Fatal("primer delivered debería aplicar")
	}
	if n.DeliveredAt == nil || !n.DeliveredAt.Equal(at) {
		t.Fatalf("delivered_at = %v, want %v", n.DeliveredAt, at)
	}
	if n.Version != v+1 {
		t.Errorf("version = %d, want %d", n.Version, v+1)
	}
	if n.MarkDelivered(at.Add(time.Minute)) {
		t.Error("callback duplicado no debe re-aplicar ni bumpear")
	}
	if n.Version != v+1 {
		t.Errorf("version cambió con el duplicado: %d", n.Version)
	}
	if got := n.DeliveryState(); got != DeliveryDelivered {
		t.Errorf("state = %q, want delivered", got)
	}
}

func TestMarkRead_ImpliesDelivered(t *testing.T) {
	n := sentNotification(t)
	at := time.Unix(2020, 0).UTC()
	if !n.MarkRead(at) {
		t.Fatal("read debería aplicar sobre sent")
	}
	if n.DeliveredAt == nil || n.ReadAt == nil {
		t.Fatalf("read sin delivered previo debe rellenar ambos: delivered=%v read=%v", n.DeliveredAt, n.ReadAt)
	}
	if got := n.DeliveryState(); got != DeliveryRead {
		t.Errorf("state = %q, want read", got)
	}
	// delivered tardío (fuera de orden) no pisa nada.
	if n.MarkDelivered(at.Add(time.Second)) {
		t.Error("delivered después de read no debe re-aplicar")
	}
}

func TestAcks_IgnoredUnlessSent(t *testing.T) {
	for _, st := range []string{StatusPending, StatusFailed, StatusHeld, StatusCancelled} {
		n := newPending(t)
		n.Status = st
		if n.MarkDelivered(time.Now()) || n.MarkRead(time.Now()) {
			t.Errorf("status %s: los acuses sólo aplican a sent", st)
		}
	}
}

func TestNewSendCycle_ClearsAcks(t *testing.T) {
	n := sentNotification(t)
	n.MarkRead(time.Unix(2030, 0).UTC())

	n.RearmForResend("+525500000000", map[string]string{}, time.Unix(3000, 0).UTC())
	if n.DeliveredAt != nil || n.ReadAt != nil {
		t.Fatal("el reenvío es un envío nuevo: no debe heredar acuses")
	}
	if got := n.DeliveryState(); got != DeliveryQueued {
		t.Errorf("state = %q, want queued", got)
	}

	// Aunque una fila pending trajera acuses viejos (el cloud los conserva
	// hasta que el dispatcher la re-envía), MarkSent los limpia.
	stale := time.Unix(2040, 0).UTC()
	n.DeliveredAt = &stale
	if err := n.MarkSent("SM456", time.Unix(3010, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if n.DeliveredAt != nil || n.ReadAt != nil {
		t.Fatal("MarkSent abre un ciclo de acuses nuevo")
	}
	if got := n.DeliveryState(); got != DeliverySent {
		t.Errorf("state = %q, want sent", got)
	}
}
