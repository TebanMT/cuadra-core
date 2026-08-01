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

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
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
	// Gyms (opcional) → "hoy" en el día LOCAL del gym (ver localToday).
	Gyms gymRepo.GymRepository
}

func NewGenderReport(reader Reader, uow sharedDomain.UnitOfWork) *GenderReport {
	return &GenderReport{Reader: reader, UoW: uow}
}

// WithGyms cablea el repo de gyms para anclar el "hoy" del reporte al día
// calendario del gym en SU zona horaria.
func (uc *GenderReport) WithGyms(g gymRepo.GymRepository) *GenderReport {
	uc.Gyms = g
	return uc
}

const defaultGenderReportDaysBack = 30

func (uc *GenderReport) Execute(ctx context.Context, in GenderReportInput) (*GenderReportOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := time.Now().UTC()
	// Día y zona del gym: today acota la composición; tzName clasifica el
	// heatmap por hora LOCAL (con hora UTC salía corrido 6 h en CDMX).
	today, tzName := localTodayAndTZ(tx, uc.Gyms, in.GymID, now)
	daysBack := in.DaysBack
	if daysBack <= 0 {
		daysBack = defaultGenderReportDaysBack
	}

	comp, err := uc.Reader.GenderComposition(tx, in.GymID, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	byHour, err := uc.Reader.AttendanceByGenderHour(tx, in.GymID, tzName, daysBack, now)
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
