package reports

import (
	"testing"
	"time"
)

// defaultRange se ancla al día calendario del GYM. El FE normalmente manda
// from/to explícitos, pero cuando no lo hace el default salía en día UTC: un
// export a las 8 PM de CDMX traía "hasta mañana", y hecho el último día del
// mes arrancaba el rango en el MES equivocado.
//
// El reloj se fija (nowUTC) porque la frontera sólo se manifiesta entre las
// 6 PM y la medianoche en CDMX: con el reloj real estos tests pasarían
// trivialmente el resto del día.

// freezeClock fija el reloj del paquete durante el test.
func freezeClock(t *testing.T, at time.Time) {
	t.Helper()
	original := nowUTC
	nowUTC = func() time.Time { return at }
	t.Cleanup(func() { nowUTC = original })
}

// 27-jul-2026 8:00 PM en CDMX — en UTC ya es el 28.
var nocheDel27UTC = time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC)

func TestDefaultRange_TerminaEnElDiaLocalDelGym(t *testing.T) {
	freezeClock(t, nocheDel27UTC)

	_, to := defaultRange("America/Mexico_City", nil, nil)
	if got := to.Format("2006-01-02"); got != "2026-07-27" {
		t.Errorf("to = %s, want 2026-07-27 (el gym sigue en el 27)", got)
	}
}

func TestDefaultRange_SinZonaCaeADiaUTC(t *testing.T) {
	freezeClock(t, nocheDel27UTC)

	_, to := defaultRange("", nil, nil)
	if got := to.Format("2006-01-02"); got != "2026-07-28" {
		t.Errorf("sin zona to = %s, want 2026-07-28 (fail-open al día UTC)", got)
	}
}

// TestDefaultRange_UltimoDiaDelMes_NoSaltaDeMes — el caso feo: 31-jul 8 PM
// en CDMX es 1-ago en UTC. Con el anclaje viejo el rango por defecto salía
// "1-ago .. 1-ago" y el export del mes llegaba VACÍO.
func TestDefaultRange_UltimoDiaDelMes_NoSaltaDeMes(t *testing.T) {
	freezeClock(t, time.Date(2026, 8, 1, 2, 0, 0, 0, time.UTC)) // 31-jul 8 PM CDMX

	from, to := defaultRange("America/Mexico_City", nil, nil)
	if got := from.Format("2006-01-02"); got != "2026-07-01" {
		t.Errorf("from = %s, want 2026-07-01", got)
	}
	if got := to.Format("2006-01-02"); got != "2026-07-31" {
		t.Errorf("to = %s, want 2026-07-31", got)
	}
}

// TestDefaultRange_RespetaLasCotasExplicitas — cuando el FE manda from/to,
// la zona no debe alterarlos.
func TestDefaultRange_RespetaLasCotasExplicitas(t *testing.T) {
	freezeClock(t, nocheDel27UTC)

	from := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)

	gotFrom, gotTo := defaultRange("America/Mexico_City", &from, &to)
	if !gotFrom.Equal(from) || !gotTo.Equal(to) {
		t.Errorf("defaultRange alteró las cotas explícitas: [%v, %v]", gotFrom, gotTo)
	}
}
