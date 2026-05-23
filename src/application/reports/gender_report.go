// Reporte operativo de género (DA-012.7). Dos agregados que el dueño
// consulta desde la página de reportes para decidir horarios dedicados —
// caso clásico "horario de mujeres" en gyms MX:
//
//  1. GenderComposition — % por bucket sobre el padrón activo.
//  2. AttendanceByGenderHour — checkins × hora × género (últimos 30 días)
//     para detectar bandas horarias donde una demografía concreta domina.
//
// Read-only (UoW.Query), single round-trip por agregado. Sin cache propio —
// el FE no consulta esto en hot path; si en algún momento lo metemos en el
// dashboard principal le pondremos cache análogo al de Dashboard.
package reports

import (
	"context"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// GenderReportInput identifica el gym a consultar. DaysBack acota la
// ventana del heatmap de asistencias; por default 30 días — más se vuelve
// ruidoso (cambios estacionales) y menos no agrega señal estadística.
type GenderReportInput struct {
	GymID    uuid.UUID
	DaysBack int
}

// GenderReportOutput es el payload que el FE renderea (donut + heatmap).
// GeneratedAt sirve para que el dueño vea "actualizado a las HH:MM" sin
// tener que crear un cache wrapper.
type GenderReportOutput struct {
	GeneratedAt time.Time                   `json:"generated_at"`
	DaysBack    int                         `json:"days_back"`
	Composition GenderCompositionRow        `json:"composition"`
	ByHour      []AttendanceByGenderHourRow `json:"by_hour"`
}

// GenderReport es el use case.
type GenderReport struct {
	Reader Reader
	UoW    sharedDomain.UnitOfWork
}

func NewGenderReport(reader Reader, uow sharedDomain.UnitOfWork) *GenderReport {
	return &GenderReport{Reader: reader, UoW: uow}
}

const defaultGenderReportDaysBack = 30

func (uc *GenderReport) Execute(ctx context.Context, in GenderReportInput) (*GenderReportOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	daysBack := in.DaysBack
	if daysBack <= 0 {
		daysBack = defaultGenderReportDaysBack
	}

	comp, err := uc.Reader.GenderComposition(tx, in.GymID, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	byHour, err := uc.Reader.AttendanceByGenderHour(tx, in.GymID, daysBack, now)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	return &GenderReportOutput{
		GeneratedAt: now,
		DaysBack:    daysBack,
		Composition: comp,
		ByHour:      byHour,
	}, nil
}
