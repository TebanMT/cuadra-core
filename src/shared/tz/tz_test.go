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

// ---------------------------------------------------------------------------
// tzdata embebida — el chequeo que faltaba
// ---------------------------------------------------------------------------

// TestTzdataEmbebida es el guardián del import `_ "time/tzdata"`.
//
// Sin él, en Windows NO hay de dónde leer las zonas horarias (el stdlib deja
// platformZoneSources vacío y las syscalls sólo alimentan a time.Local), así
// que LoadLocation fallaba SIEMPRE en el sidecar y todo el paquete caía a UTC
// en silencio: las fechas locales del gym eran letra muerta en la PC de
// recepción y sólo funcionaban en el cloud.
//
// Este test corre en Linux/macOS, donde la zona cargaría igual desde
// /usr/share/zoneinfo — no puede distinguir ambas fuentes. Su valor es de
// contrato: si alguien quita el import, TestZonasSoportadasDelFEcargan es
// quien truena en CI de Windows, y este documenta el porqué.
func TestTzdataEmbebida(t *testing.T) {
	if err := Verify("America/Mexico_City"); err != nil {
		t.Fatalf("la base de zonas horarias no está disponible: %v", err)
	}
}

// TestZonasSoportadasDelFEcargan — el Select de Ajustes ofrece estas 11
// zonas; todas tienen que resolver. Una que no cargue degrada a UTC en
// silencio y corre las fechas del gym que la eligió.
func TestZonasSoportadasDelFEcargan(t *testing.T) {
	zonas := []string{
		"America/Mexico_City", "America/Cancun", "America/Chihuahua",
		"America/Hermosillo", "America/Mazatlan", "America/Monterrey",
		"America/Tijuana", "America/Merida", "America/Bahia_Banderas",
		"America/Matamoros", "America/Ojinaga",
	}
	for _, z := range zonas {
		if err := Verify(z); err != nil {
			t.Errorf("zona %q del Select del FE no carga: %v", z, err)
		}
	}
}

// ---------------------------------------------------------------------------
// DayBounds / OffsetSeconds / NameOrUTC
// ---------------------------------------------------------------------------

// TestDayBounds_DiaLocalEsRangoDeInstantes — el día local 27-jul en CDMX va
// de las 06:00Z de ese día a las 06:00Z del 28, no de medianoche a
// medianoche UTC. Ese corrimiento de 6 h es el que mandaba el horario pico
// del gym al día siguiente.
func TestDayBounds_DiaLocalEsRangoDeInstantes(t *testing.T) {
	d := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	start, end := DayBounds("America/Mexico_City", d, d)

	if want := time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("start = %v, want %v", start, want)
	}
	if want := time.Date(2026, 7, 28, 6, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Errorf("end = %v, want %v", end, want)
	}
}

// TestDayBounds_ContieneLaNocheLocal — la prueba de fuego: un check-in de
// las 7:30 PM del 27 (01:30Z del 28) cae DENTRO del día local 27.
func TestDayBounds_ContieneLaNocheLocal(t *testing.T) {
	d := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	start, end := DayBounds("America/Mexico_City", d, d)

	nocturno := time.Date(2026, 7, 28, 1, 30, 0, 0, time.UTC)
	if nocturno.Before(start) || !nocturno.Before(end) {
		t.Errorf("el instante nocturno %v quedó fuera de [%v, %v)", nocturno, start, end)
	}
}

// TestDayBounds_RangoMultiDia — extremos inclusivos como días, fin exclusivo
// como instante.
func TestDayBounds_RangoMultiDia(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	start, end := DayBounds("America/Mexico_City", from, to)

	if want := time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("start = %v, want %v", start, want)
	}
	// Fin = medianoche local del 1-ago, o sea el 31 completo queda dentro.
	if want := time.Date(2026, 8, 1, 6, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Errorf("end = %v, want %v", end, want)
	}
}

// TestDayBounds_SinZonaEsElDiaUTC — fail-open idéntico al comportamiento
// previo.
func TestDayBounds_SinZonaEsElDiaUTC(t *testing.T) {
	d := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"", "No/Existe"} {
		start, end := DayBounds(name, d, d)
		if !start.Equal(d) || !end.Equal(d.AddDate(0, 0, 1)) {
			t.Errorf("DayBounds(%q) = [%v, %v), want el día UTC", name, start, end)
		}
	}
}

func TestOffsetSeconds(t *testing.T) {
	at := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if got := OffsetSeconds("America/Mexico_City", at); got != -6*3600 {
		t.Errorf("offset CDMX = %d, want -21600", got)
	}
	// Tijuana es UTC-7 en verano (sí observa DST, a diferencia del resto
	// del país) — el caso que justifica calcular el offset por instante.
	if got := OffsetSeconds("America/Tijuana", at); got != -7*3600 {
		t.Errorf("offset Tijuana en julio = %d, want -25200", got)
	}
	if got := OffsetSeconds("No/Existe", at); got != 0 {
		t.Errorf("offset de zona inválida = %d, want 0 (UTC)", got)
	}
}

// TestNameOrUTC — Postgres resuelve zonas con su propio catálogo y un
// nombre basura ahí no cae a UTC: revienta el query. Se valida antes.
func TestNameOrUTC(t *testing.T) {
	if got := NameOrUTC("America/Mexico_City"); got != "America/Mexico_City" {
		t.Errorf("NameOrUTC(válida) = %q", got)
	}
	for _, bad := range []string{"", "No/Existe", "'; DROP TABLE gyms; --"} {
		if got := NameOrUTC(bad); got != "UTC" {
			t.Errorf("NameOrUTC(%q) = %q, want UTC", bad, got)
		}
	}
}
