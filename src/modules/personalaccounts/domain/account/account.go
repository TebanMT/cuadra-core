// Package account contains the personal profile aggregate. Phone ownership,
// credentials and security transitions use separate commands; profile updates
// have no access to those fields or to historical workout measurements.
package account

import (
	"math"
	"strings"
	"time"
	"unicode/utf8"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/google/uuid"
)

type WeightUnit string

const (
	Kilograms WeightUnit = "kg"
	Pounds    WeightUnit = "lb"
)

type Status string

const (
	Active          Status = "active"
	DeletionPending Status = "deletion_pending"
	Deleted         Status = "deleted"
)

type Account struct {
	ID          uuid.UUID
	DisplayName string
	WeightUnit  WeightUnit
	Revision    int64
	Status      Status
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// NewAccount represents a newly authorized person before completing the
// profile. The S00 read shape permits the initial empty name; profile.update
// does not. This constructor does not create an identity or authorize access.
func NewAccount(id uuid.UUID, now time.Time) (*Account, error) {
	a := &Account{ID: id, WeightUnit: Kilograms, Revision: 1, Status: Active, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Account) Validate() error {
	if a == nil || a.ID == uuid.Nil || a.ID.Variant() != uuid.RFC4122 || a.ID.Version() < 1 || a.ID.Version() > 8 ||
		a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		return personalErrors.ErrInvalidAccount
	}
	if !utf8.ValidString(a.DisplayName) || strings.ContainsRune(a.DisplayName, 0) || utf8.RuneCountInString(a.DisplayName) > 80 {
		return personalErrors.ErrInvalidDisplayName
	}
	if a.WeightUnit != Kilograms && a.WeightUnit != Pounds {
		return personalErrors.ErrInvalidWeightUnit
	}
	if a.Revision < 1 {
		return personalErrors.ErrInvalidProfileRevision
	}
	switch a.Status {
	case Active, DeletionPending, Deleted:
	default:
		return personalErrors.ErrInvalidAccount
	}
	return nil
}

func ValidateProfile(displayName string, weightUnit WeightUnit) error {
	if !utf8.ValidString(displayName) || strings.ContainsRune(displayName, 0) || utf8.RuneCountInString(displayName) > 80 || strings.TrimSpace(displayName) == "" {
		return personalErrors.ErrInvalidDisplayName
	}
	if weightUnit != Kilograms && weightUnit != Pounds {
		return personalErrors.ErrInvalidWeightUnit
	}
	return nil
}

// UpdateProfile validates everything before changing the aggregate. The SQL
// repository repeats CAS against persisted revision; this check catches stale
// commands already visible in the loaded aggregate without making a write.
func (a *Account) UpdateProfile(displayName string, weightUnit WeightUnit, expectedRevision int64, now time.Time) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.Status != Active || a.DeletedAt != nil {
		return personalErrors.ErrAccountUnavailable
	}
	if expectedRevision < 1 {
		return personalErrors.ErrInvalidProfileRevision
	}
	if a.Revision != expectedRevision {
		return personalErrors.ErrProfileRevisionConflict
	}
	if a.Revision == math.MaxInt64 {
		return personalErrors.ErrProfileRevisionExhausted
	}
	if now.IsZero() {
		return personalErrors.ErrInvalidAccount
	}
	if err := ValidateProfile(displayName, weightUnit); err != nil {
		return err
	}
	a.DisplayName, a.WeightUnit = strings.TrimSpace(displayName), weightUnit
	a.Revision++
	a.UpdatedAt = now.UTC()
	return nil
}

// ValidateProfileWrite protects the repository boundary from a manually built
// or mutated aggregate. Only the next revision of an active profile can be
// persisted; callers cannot skip revisions or clear a completed name.
func (a *Account) ValidateProfileWrite(expectedRevision int64) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.Status != Active || a.DeletedAt != nil {
		return personalErrors.ErrAccountUnavailable
	}
	if expectedRevision < 1 {
		return personalErrors.ErrInvalidProfileRevision
	}
	if expectedRevision == math.MaxInt64 {
		return personalErrors.ErrProfileRevisionExhausted
	}
	if a.Revision != expectedRevision+1 {
		return personalErrors.ErrInvalidProfileRevision
	}
	if err := ValidateProfile(a.DisplayName, a.WeightUnit); err != nil {
		return err
	}
	if a.DisplayName != strings.TrimSpace(a.DisplayName) {
		return personalErrors.ErrInvalidDisplayName
	}
	return nil
}
