package tz

import (
	"testing"
	"time"
)

func TestLocalToday_NocheLocalNoCruzaAlDiaUTC(t *testing.T) {
	// 6-jul 10:00 PM en CDMX == 7-jul 04:00 UTC. El día de negocio sigue
	// siendo el 6 — este es exactamente el bug del cobro nocturno.
	now := time.Date(2026, 7, 7, 4, 0, 0, 0, time.UTC)
	got := LocalToday("America/Mexico_City", now)
	want := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("LocalToday = %v, want %v", got, want)
	}
}

func TestLocalToday_MananaLocalCoincideConUTC(t *testing.T) {
	// 6-jul 10:00 AM CDMX == 6-jul 16:00 UTC — mismo día en ambos.
	now := time.Date(2026, 7, 6, 16, 0, 0, 0, time.UTC)
	got := LocalToday("America/Mexico_City", now)
	want := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("LocalToday = %v, want %v", got, want)
	}
}

func TestLocalToday_TzInvalidaCaeAUTC(t *testing.T) {
	now := time.Date(2026, 7, 7, 4, 0, 0, 0, time.UTC)
	for _, tzName := range []string{"", "No/Existe"} {
		got := LocalToday(tzName, now)
		want := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("LocalToday(%q) = %v, want fallback UTC %v", tzName, got, want)
		}
	}
}
