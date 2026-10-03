package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// resolvePurchasePaidOn treats paid_on as the gym's economic calendar date.
// The UI's max=today is only convenience: API and offline sync must reject a
// future payment authoritatively while still allowing legitimate backdating.
func resolvePurchasePaidOn(tx sharedDomain.Transaction, gyms gymRepo.GymRepository,
	gymID uuid.UUID, requested, now time.Time) (time.Time, error) {
	today, err := purchaseLocalToday(tx, gyms, gymID, now)
	if err != nil {
		return time.Time{}, err
	}
	day := purchaseDateOnly(requested)
	if requested.IsZero() || day.After(today) {
		return time.Time{}, sharedDomain.NewValidationError(prodErrors.ErrPurchaseDateInvalid)
	}
	return day, nil
}

func purchaseLocalToday(tx sharedDomain.Transaction, gyms gymRepo.GymRepository,
	gymID uuid.UUID, now time.Time) (time.Time, error) {
	location, err := purchaseLocation(tx, gyms, gymID)
	if err != nil {
		return time.Time{}, err
	}
	return tz.LocalToday(location.String(), now), nil
}

func purchaseLocation(tx sharedDomain.Transaction, gyms gymRepo.GymRepository, gymID uuid.UUID) (*time.Location, error) {
	if gyms == nil {
		return time.UTC, nil
	}
	gym, err := gyms.GetByID(tx, gymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if gym == nil {
		return nil, sharedDomain.NewUnexpectedError(fmt.Errorf("gym %s is unavailable", gymID))
	}
	zone := strings.TrimSpace(gym.Timezone)
	if zone == "" {
		return nil, sharedDomain.NewUnexpectedError(fmt.Errorf("gym %s has no timezone", gymID))
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(fmt.Errorf("gym %s has invalid timezone %q: %w", gymID, zone, err))
	}
	return location, nil
}
