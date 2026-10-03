package reports

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// localToday resuelve el "hoy" de los reportes: el día calendario del GYM
// en su zona horaria, no el día UTC. Los reportes corren en AMBOS binarios
// (el dashboard del desktop consume el sidecar; el del dueño, el cloud) y
// con el anclaje UTC anterior, desde las 6 PM de CDMX los KPIs de "hoy",
// las fronteras de mes y las listas de por-vencer mostraban el día
// corrido. Espejo de gymLocalPaymentDate (billing) y gymLocalToday
// (checkins) — mismo bug, mismo fix.
//
// Fail-open: sin repo cableado, gym inexistente o tz inválida → día UTC
// (comportamiento previo).
func localToday(
	tx sharedDomain.Transaction,
	gyms gymRepo.GymRepository,
	gymID uuid.UUID,
	now time.Time,
) time.Time {
	today, _ := localTodayAndTZ(tx, gyms, gymID, now)
	return today
}

// localTodayAndTZ es localToday más el nombre de la zona, resuelto en la
// MISMA lectura del gym. Los reportes necesitan ambos: el día local para
// acotar el período y la zona para que las columnas de TIMESTAMP
// (checkin_at, created_at) se agrupen por el día del gym y no por el de
// UTC — ver tz.DayBounds. Zona vacía = UTC, la misma semántica fail-open.
func localTodayAndTZ(
	tx sharedDomain.Transaction,
	gyms gymRepo.GymRepository,
	gymID uuid.UUID,
	now time.Time,
) (time.Time, string) {
	if gyms == nil {
		return truncateUTC(now), ""
	}
	g, err := gyms.GetByID(tx, gymID)
	if err != nil || g == nil {
		return truncateUTC(now), ""
	}
	return tz.LocalToday(g.Timezone, now), g.Timezone
}

// cashCommandLocalTodayAndTZ is the strict calendar resolver for commands
// that certify or modify a physical cash session. A report may remain
// available with the historical UTC fallback when the gym lookup is
// temporarily unavailable, but a mutation must never guess the operational
// day: around midnight UTC that could certify tomorrow in the gym's calendar.
//
// A nil repository is retained as an explicit compatibility mode for legacy
// tests/adapters that have not wired gyms yet. Once a repository is wired,
// lookup failures, missing gyms and invalid timezones all fail closed.
func cashCommandLocalTodayAndTZ(
	tx sharedDomain.Transaction,
	gyms gymRepo.GymRepository,
	gymID uuid.UUID,
	now time.Time,
) (time.Time, string, error) {
	if gyms == nil {
		return truncateUTC(now), "", nil
	}
	g, err := gyms.GetByID(tx, gymID)
	if err != nil {
		return time.Time{}, "", err
	}
	if g == nil {
		return time.Time{}, "", fmt.Errorf("gym %s is unavailable", gymID)
	}
	timezone := strings.TrimSpace(g.Timezone)
	if timezone == "" {
		return time.Time{}, "", fmt.Errorf("gym %s has no timezone", gymID)
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return time.Time{}, "", fmt.Errorf("gym %s has invalid timezone %q: %w", gymID, timezone, err)
	}
	return tz.LocalToday(timezone, now), timezone, nil
}

// nowUTC es el reloj del paquete. Variable y no llamada directa para que
// los tests puedan fijar la hora: las fronteras de día sólo se manifiestan
// en una ventana concreta (en CDMX, de 6 PM a medianoche), así que un test
// que dependa del reloj real pasa trivialmente el 75% del día y da una
// falsa sensación de cobertura.
var nowUTC = func() time.Time { return time.Now().UTC() }

func truncateUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
