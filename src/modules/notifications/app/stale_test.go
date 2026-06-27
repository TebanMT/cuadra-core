package app

import (
	"testing"
	"time"
)

func TestStaleTTL_PorTemplate(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		key  string
		want time.Duration
	}{
		{"owner_alert_cash_close_diff", 2 * day},
		{"owner_alert_no_payments_today", 2 * day},
		{"receipt_membership", 1 * day},
		{"receipt_product", 1 * day},
		{"member_welcome_number", 1 * day},
		{"operator_welcome_pin", 1 * day},
		{"owner_welcome", 1 * day},
		{"broadcast_freeform", 1 * day}, // Marketing → time-sensitive
		{"algo_desconocido", 14 * day},  // sin def → default amplio
	}
	for _, c := range cases {
		if got := staleTTL(c.key); got != c.want {
			t.Errorf("staleTTL(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

func TestIsStale_AnclaEnCreatedAt(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	// receipt TTL = 1 día: un recibo del mismo día sí, de ayer no.
	if isStale("receipt_membership", base, base.Add(12*time.Hour)) {
		t.Errorf("12h < 1d TTL → no debería ser stale")
	}
	if !isStale("receipt_membership", base, base.Add(2*24*time.Hour)) {
		t.Errorf("2 días > 1d TTL → debería ser stale")
	}
	// owner_alert TTL = 2 días: un outage de 3 días ya lo retiene.
	if !isStale("owner_alert_cash_close_diff", base, base.Add(3*24*time.Hour)) {
		t.Errorf("alerta a 3 días debería ser stale (TTL 2d)")
	}
	// Bienvenida TTL = 1 día (incluye el PIN de login del operador).
	if isStale("operator_welcome_pin", base, base.Add(12*time.Hour)) {
		t.Errorf("bienvenida a 12h NO debería ser stale (TTL 1d)")
	}
	if !isStale("member_welcome_number", base, base.Add(2*24*time.Hour)) {
		t.Errorf("bienvenida a 2 días debería ser stale (TTL 1d)")
	}
}
