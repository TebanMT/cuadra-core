package app

import (
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// gymLocalPaymentDate resuelve el default de PaymentDate cuando el caller
// no mandó fecha: el día calendario del GYM en su zona horaria, no el día
// UTC. Sin esto, un cobro a las 10 PM de CDMX (04:00 UTC del día
// siguiente) caía en el día equivocado: vencimiento de la renovación
// corrido +1 y la venta/el cobro en la caja del día siguiente. Espejo del
// gymLocalToday de checkins (mismo bug, mismo fix).
//
// Fail-open: sin repo cableado, gym inexistente o tz inválida → día UTC
// (comportamiento previo). El desktop normalmente manda payment_date
// explícito (fecha local de la PC del gym) — este default cubre a los
// callers que no lo mandan (venta rápida, cobro rápido, refunds).
func gymLocalPaymentDate(
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
