// Package membership_type holds the MembershipType aggregate (UC-011).
// Cambios de precio/duración NO son retroactivos: las membresías activas
// conservan su snapshot — ver DA-11.1 + ADR-002 §3.6.
package membership_type

import (
	"strings"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
)

// FrequencyXxx mirror chk_membership_types_frequency. Empty string means
// "no maintenance fee" — domain represents that as nil. Bimonthly /
// quarterly / semiannual se agregaron en migration 010 (postgres) /
// 007 (sqlite) para cubrir gimnasios que cobran mantenimiento cada
// 2/3/6 meses.
const (
	FrequencyMonthly    = "monthly"
	FrequencyBimonthly  = "bimonthly"
	FrequencyQuarterly  = "quarterly"
	FrequencySemiannual = "semiannual"
	FrequencyAnnual     = "annual"
)

// allowedFrequencies — set de valores que el CHECK del schema acepta.
// Si crece, debe sincronizarse con las migrations correspondientes.
var allowedFrequencies = map[string]struct{}{
	FrequencyMonthly:    {},
	FrequencyBimonthly:  {},
	FrequencyQuarterly:  {},
	FrequencySemiannual: {},
	FrequencyAnnual:     {},
}

// MembershipType is a plan offered by the gym. Price is stored as float64 here
// for simplicity; cents-only is enforced at the SQLite mapper boundary.
//
// DurationDays vs DurationMonths: el operador de un gym ve "mensual" como
// un período de calendario (1 mes natural), no como 30 días corridos. Si
// DurationMonths != nil, ese campo manda y el expiry se calcula como
// start + N meses naturales (lo que tiene el mes en la realidad: 28-31
// días). Si DurationMonths es nil, caemos a DurationDays — usado por
// presets exactos (paso/semana/quincenal) + duración personalizada.
// Migración 027/022 hace el backfill de los presets ambiguos
// {30,60,90,180,365} a sus equivalentes en meses {1,2,3,6,12}.
type MembershipType struct {
	ID                   uuid.UUID
	GymID                uuid.UUID
	Version              int
	Name                 string
	Price                float64
	DurationDays         int
	DurationMonths       *int
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
//
// durationMonths puede ser nil (use cases legacy + presets de días).
// Si llega NO-nil, manda sobre durationDays para el cálculo de expiry
// — pero el caller TAMBIÉN debe pasar un durationDays "aproximado" (su
// valor histórico), porque algunos reportes legacy + el wire DTO lo
// siguen exponiendo.
func New(id, gymID uuid.UUID, name string, price float64, durationDays int, durationMonths *int,
	enrollmentFee, maintenanceFee float64, freq string, now time.Time) (*MembershipType, error) {
	mt := &MembershipType{
		ID:        id,
		GymID:     gymID,
		Version:   1,
		Active:    true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := mt.applyFields(name, price, durationDays, durationMonths, enrollmentFee, maintenanceFee, freq); err != nil {
		return nil, err
	}
	return mt, nil
}

// Update mutates the MembershipType in-place. Per DA-11.1 the caller is NOT
// required to update existing memberships — those keep their snapshot. Only
// future renewals see the new values.
func (mt *MembershipType) Update(name string, price float64, durationDays int, durationMonths *int,
	enrollmentFee, maintenanceFee float64, freq string, now time.Time) error {
	if err := mt.applyFields(name, price, durationDays, durationMonths, enrollmentFee, maintenanceFee, freq); err != nil {
		return err
	}
	mt.Version++
	mt.UpdatedAt = now
	return nil
}

// Deactivate is the soft-delete path (DA-11.2). active=false; DeletedAt stays
// nil because legacy payments still need to reference the row.
func (mt *MembershipType) Deactivate(now time.Time) {
	if !mt.Active {
		return
	}
	mt.Active = false
	mt.Version++
	mt.UpdatedAt = now
}

// Reactivate is symmetric. Used when an owner toggles the plan back on.
func (mt *MembershipType) Reactivate(now time.Time) {
	if mt.Active {
		return
	}
	mt.Active = true
	mt.Version++
	mt.UpdatedAt = now
}

func (mt *MembershipType) applyFields(name string, price float64, durationDays int, durationMonths *int,
	enrollmentFee, maintenanceFee float64, freq string) error {
	name = strings.TrimSpace(name)
	if len(name) < 3 || len(name) > 100 {
		return memErrors.ErrInvalidMembershipTypeName
	}
	if price <= 0 {
		return memErrors.ErrInvalidPrice
	}
	if durationDays < 1 {
		return memErrors.ErrInvalidDuration
	}
	if durationMonths != nil && (*durationMonths < 1 || *durationMonths > 60) {
		return memErrors.ErrInvalidDuration
	}
	if enrollmentFee < 0 || maintenanceFee < 0 {
		return memErrors.ErrInvalidPrice
	}
	var freqPtr *string
	switch {
	case maintenanceFee == 0:
		// Frequency must be NULL when there's no fee (chk constraint).
	case maintenanceFee > 0:
		if _, ok := allowedFrequencies[freq]; !ok {
			return memErrors.ErrInvalidMaintenanceFreq
		}
		f := freq
		freqPtr = &f
	}
	mt.Name = name
	mt.Price = price
	mt.DurationDays = durationDays
	if durationMonths != nil {
		v := *durationMonths
		mt.DurationMonths = &v
	} else {
		mt.DurationMonths = nil
	}
	mt.EnrollmentFee = enrollmentFee
	mt.MaintenanceFee = maintenanceFee
	mt.MaintenanceFrequency = freqPtr
	return nil
}

// Validator is the chain interface for create-time validation.
type Validator interface {
	Validate(mt *MembershipType) error
}

type nameValidator struct{ Next Validator }

func (v *nameValidator) Validate(mt *MembershipType) error {
	if len(strings.TrimSpace(mt.Name)) < 3 || len(mt.Name) > 100 {
		return memErrors.ErrInvalidMembershipTypeName
	}
	if v.Next != nil {
		return v.Next.Validate(mt)
	}
	return nil
}

type priceValidator struct{ Next Validator }

func (v *priceValidator) Validate(mt *MembershipType) error {
	if mt.Price <= 0 {
		return memErrors.ErrInvalidPrice
	}
	if mt.EnrollmentFee < 0 || mt.MaintenanceFee < 0 {
		return memErrors.ErrInvalidPrice
	}
	if v.Next != nil {
		return v.Next.Validate(mt)
	}
	return nil
}

type durationValidator struct{ Next Validator }

func (v *durationValidator) Validate(mt *MembershipType) error {
	if mt.DurationDays < 1 {
		return memErrors.ErrInvalidDuration
	}
	if v.Next != nil {
		return v.Next.Validate(mt)
	}
	return nil
}

func BuildValidatorChain() Validator {
	return &nameValidator{Next: &priceValidator{Next: &durationValidator{}}}
}
