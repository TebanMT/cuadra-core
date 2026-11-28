//go:build sidecar

package sync

import (
	"testing"
	"time"
)

// Pin del 500 del historial de WhatsApp (nov-2026): los recordatorios que
// genera el cloud llegan con scheduled_for como string RFC3339. El apply
// sólo convertía columnas *_at y lo escribía TAL CUAL en la columna INTEGER.
// Ahora usa el mismo catálogo que el projector.
func TestExtractColumnValue_TimestamptzSinSufijoAt_SeConvierteAEpochMs(t *testing.T) {
	pl := map[string]any{
		"scheduled_for":           "2026-08-31T17:42:37.960967-06:00",
		"measurement_t0_deadline": "2026-09-01T06:00:00Z",
		"template_key":            "expiry_reminder_today",
	}
	want := time.Date(2026, 8, 31, 23, 42, 37, 960_967_000, time.UTC).UnixMilli()
	if got := extractColumnValue(pl, "notification_queue", "scheduled_for"); got != want {
		t.Fatalf("scheduled_for = %#v, want %d", got, want)
	}
	if got := extractColumnValue(pl, "challenges", "measurement_t0_deadline"); got != time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("measurement_t0_deadline = %#v", got)
	}
	// Una columna de texto normal no se toca aunque parezca fecha.
	if got := extractColumnValue(pl, "notification_queue", "template_key"); got != "expiry_reminder_today" {
		t.Fatalf("template_key = %#v", got)
	}
	// El catálogo es por tabla: scheduled_for de otra tabla no se convierte.
	if isTimestampColumn("payments", "scheduled_for") {
		t.Fatal("scheduled_for sólo es instante en notification_queue")
	}
}
