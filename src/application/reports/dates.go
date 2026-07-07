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
	if gyms == nil {
		return truncateUTC(now)
	}
	g, err := gyms.GetByID(tx, gymID)
	if err != nil || g == nil {
		return truncateUTC(now)
	}
	return tz.LocalToday(g.Timezone, now)
}

func truncateUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
