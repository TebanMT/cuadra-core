package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
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

// strictGymLocalPaymentDate resolves the authoritative economic calendar for
// money-writing commands. Once the gyms repository is wired, falling back to
// UTC on a lookup/configuration error would silently move late-night money to
// tomorrow for western timezones. Nil remains an explicit compatibility mode
// for legacy adapters that have not wired the repository yet.
func strictGymLocalPaymentDate(
	tx sharedDomain.Transaction,
	gyms gymRepo.GymRepository,
	gymID uuid.UUID,
	now time.Time,
) (time.Time, error) {
	if gyms == nil {
		return truncateUTC(now), nil
	}
	g, err := gyms.GetByID(tx, gymID)
	if err != nil {
		return time.Time{}, sharedDomain.NewUnexpectedError(err)
	}
	if g == nil {
		return time.Time{}, sharedDomain.NewUnexpectedError(fmt.Errorf("gym %s is unavailable", gymID))
	}
	zone := strings.TrimSpace(g.Timezone)
	if zone == "" {
		return time.Time{}, sharedDomain.NewUnexpectedError(fmt.Errorf("gym %s has no timezone", gymID))
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return time.Time{}, sharedDomain.NewUnexpectedError(fmt.Errorf("gym %s has invalid timezone %q: %w", gymID, zone, err))
	}
	return tz.LocalToday(zone, now), nil
}

func truncateUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// resolveMonetaryDate normalizes an economic date and rejects tomorrow (or
// later) relative to the gym's local calendar. UI max=today is convenience;
// this application-layer guard is authoritative for API and offline sync.
func resolveMonetaryDate(
	tx sharedDomain.Transaction,
	gyms gymRepo.GymRepository,
	gymID uuid.UUID,
	requested, now time.Time,
) (time.Time, error) {
	today, err := strictGymLocalPaymentDate(tx, gyms, gymID, now)
	if err != nil {
		return time.Time{}, err
	}
	if requested.IsZero() {
		return today, nil
	}
	day := monetaryDateOnly(requested)
	if day.After(today) {
		return time.Time{}, sharedDomain.NewValidationError(billingErrors.ErrPaymentDateInvalid)
	}
	return day, nil
}

// validateMonetaryChronology prevents a child money event from preceding the
// collection that gives it meaning. Same-day settlement/refund is valid.
func validateMonetaryChronology(day, sourceDay time.Time) error {
	if monetaryDateOnly(day).Before(monetaryDateOnly(sourceDay)) {
		return sharedDomain.NewValidationError(billingErrors.ErrPaymentDateInvalid)
	}
	return nil
}

func monetaryDateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
