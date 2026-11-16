// Package exercise owns personal exercise identity and descriptive content.
// It does not authorize sessions, publish routines or rewrite historical snapshots.
package exercise

import (
	"strings"
	"time"
	"unicode/utf8"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/google/uuid"
)

type LoadMode string

const (
	External LoadMode = "external"
	None     LoadMode = "none"
)

type Exercise struct {
	ID, AccountID         uuid.UUID
	Name                  string
	LoadMode              LoadMode
	Notes                 string
	Revision              int64
	CreatedAt, UpdatedAt  time.Time
	ArchivedAt, DeletedAt *time.Time
}

func NewPersonal(id, ownerID uuid.UUID, name string, loadMode LoadMode, notes string, now time.Time) (*Exercise, error) {
	e := &Exercise{ID: id, AccountID: ownerID, Name: name, LoadMode: loadMode, Notes: notes, Revision: 1,
		CreatedAt: now.UTC().Truncate(time.Millisecond), UpdatedAt: now.UTC().Truncate(time.Millisecond)}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return e, nil
}

func validID(id uuid.UUID) bool {
	return id != uuid.Nil && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
}
func validText(s string, min, max int) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0) && utf8.RuneCountInString(s) >= min && utf8.RuneCountInString(s) <= max
}

func ValidateFields(name string, mode LoadMode, notes string) error {
	if !validText(name, 1, 120) || strings.TrimSpace(name) == "" || !validText(notes, 0, 1000) || (mode != External && mode != None) {
		return trainingErrors.ErrInvalidExercise
	}
	return nil
}

func (e *Exercise) Validate() error {
	if e == nil || !validID(e.ID) || !validID(e.AccountID) || e.Revision < 1 || e.CreatedAt.IsZero() || e.CreatedAt.Year() < 1 || e.CreatedAt.Year() > 9999 || e.UpdatedAt.Before(e.CreatedAt) || e.UpdatedAt.Year() > 9999 {
		return trainingErrors.ErrInvalidExercise
	}
	if err := ValidateFields(e.Name, e.LoadMode, e.Notes); err != nil {
		return err
	}
	for _, at := range []*time.Time{e.ArchivedAt, e.DeletedAt} {
		if at != nil && (at.Before(e.CreatedAt) || at.Year() > 9999) {
			return trainingErrors.ErrInvalidExercise
		}
	}
	return nil
}

func (e *Exercise) ValidateCreation(ownerID uuid.UUID) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if e.AccountID != ownerID || e.Revision != 1 || !e.UpdatedAt.Equal(e.CreatedAt) || e.ArchivedAt != nil || e.DeletedAt != nil || e.CreatedAt.Nanosecond()%int(time.Millisecond) != 0 {
		return trainingErrors.ErrInvalidExercise
	}
	return nil
}
