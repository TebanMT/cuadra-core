// Package membership_type holds the MembershipType aggregate. Sesión 1
// implements just enough for UC-001 step 3 — full CRUD lives in Sesión 2.
package membership_type

import (
	"strings"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
)

// FrequencyMonthly / FrequencyAnnual mirror chk_membership_types_frequency.
// Empty string means "no maintenance fee" — domain represents that as nil.
const (
	FrequencyMonthly = "monthly"
	FrequencyAnnual  = "annual"
)

// MembershipType is a plan offered by the gym. Price is stored as float64 here
// for simplicity; cents-only is enforced at the SQLite mapper boundary.
type MembershipType struct {
	ID                   uuid.UUID
	GymID                uuid.UUID
	Version              int
	Name                 string
	Price                float64
	DurationDays         int
	EnrollmentFee        float64
	MaintenanceFee       float64
	MaintenanceFrequency *string
	Active               bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
	DeletedAt            *time.Time
}

// New constructs a MembershipType with consistent maintenance constraints.
// freq is either "monthly", "annual", or empty when there's no fee.
func New(id, gymID uuid.UUID, name string, price float64, durationDays int,
	enrollmentFee, maintenanceFee float64, freq string, now time.Time) (*MembershipType, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return nil, memErrors.ErrInvalidMembershipTypeName
	}
	if price <= 0 {
		return nil, memErrors.ErrInvalidPrice
	}
	if durationDays < 1 {
		return nil, memErrors.ErrInvalidDuration
	}
	var freqPtr *string
	switch {
	case maintenanceFee == 0:
		// Frequency must be NULL when there's no fee (chk constraint).
	case maintenanceFee > 0 && (freq == FrequencyMonthly || freq == FrequencyAnnual):
		f := freq
		freqPtr = &f
	default:
		return nil, memErrors.ErrInvalidMaintenanceFreq
	}
	return &MembershipType{
		ID:                   id,
		GymID:                gymID,
		Version:              1,
		Name:                 name,
		Price:                price,
		DurationDays:         durationDays,
		EnrollmentFee:        enrollmentFee,
		MaintenanceFee:       maintenanceFee,
		MaintenanceFrequency: freqPtr,
		Active:               true,
		CreatedAt:            now,
		UpdatedAt:            now,
	}, nil
}
