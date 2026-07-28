package reports

import (
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
