//go:build sidecar

// package app (interno) para poder probar los helpers no exportados del
// opt-out. Conviven con los tests package app_test del mismo directorio.
package app

import (
	"testing"

	"github.com/cuadra/cuadra-core/src/shared/phone"
)

func TestOptOutIntent(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"STOP", intentStop},
		{"stop", intentStop},
		{"Baja", intentStop},
		{"BAJA por favor", intentStop},
		{"CANCELAR", intentStop},
		{"ALTO", intentStop},
		{"trabaja en el gym", intentNone}, // NO falso positivo de "baja"
		{"ALTA", intentStart},
		{"start de nuevo", intentStart},
		{"hola, quiero info", intentNone},
		{"", intentNone},
	}
	for _, c := range cases {
		if got := optOutIntent(c.in); got != c.want {
			t.Errorf("optOutIntent(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestNormalizeInboundPhone_StripsWhatsappPrefix(t *testing.T) {
	got := normalizeInboundPhone("whatsapp:+524429876543")
	want := phone.Normalize("+524429876543")
	if got == "" || got != want {
		t.Errorf("normalizeInboundPhone = %q, want %q", got, want)
	}
}

func TestIsMarketingTemplate(t *testing.T) {
	if !isMarketingTemplate("broadcast_freeform") {
		t.Error("broadcast_freeform debe ser Marketing (sujeto a opt-out)")
	}
	for _, k := range []string{"receipt_membership", "expiry_reminder_3d", "owner_alert_low_stock"} {
		if isMarketingTemplate(k) {
			t.Errorf("%s NO debe ser Marketing (transaccional/utility, no opt-out)", k)
		}
	}
}
