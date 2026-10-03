package app

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type OperationalPolicy struct {
	// DayLock is retained while old callers and sidecars are upgraded. A cash
	// close is a certification boundary, not a write lock: new late physical
	// events are accepted and the cash-session reader marks them stale or
	// uncovered. Fund/external expenses never touch the drawer at all.
	DayLock expRepo.CashDayLock
	Gyms    gymRepo.GymRepository
}

func (p OperationalPolicy) localToday(tx shared.Transaction, gymID uuid.UUID, now time.Time) (time.Time, error) {
	zone := "America/Mexico_City"
	if p.Gyms != nil {
		g, err := p.Gyms.GetByID(tx, gymID)
		if err != nil {
			return time.Time{}, shared.NewUnexpectedError(err)
		}
		if g == nil {
			return time.Time{}, shared.NewUnexpectedError(fmt.Errorf("gym %s is unavailable", gymID))
		}
		if strings.TrimSpace(g.Timezone) == "" {
			return time.Time{}, shared.NewUnexpectedError(fmt.Errorf("gym %s has no timezone", gymID))
		}
		zone = strings.TrimSpace(g.Timezone)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, shared.NewUnexpectedError(err)
	}
	x := now.In(loc)
	return time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, time.UTC), nil
}
func (p OperationalPolicy) validatePaidDate(tx shared.Transaction, gymID uuid.UUID, day, now time.Time) error {
	d := dateOnly(day)
	today, err := p.localToday(tx, gymID, now)
	if err != nil {
		return err
	}
	if d.After(today) {
		return shared.NewValidationError(expErrors.ErrFutureDate)
	}
	return nil
}
func dateOnly(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}

func normalizedCorrectionReason(raw string) (string, error) {
	reason := strings.TrimSpace(raw)
	if utf8.RuneCountInString(reason) < 3 || utf8.RuneCountInString(reason) > 200 {
		return "", shared.NewValidationError(expErrors.ErrCorrectionReasonRequired)
	}
	return reason, nil
}
