package exercise

import (
	"strings"
	"time"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/search"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

const (
	CatalogSource  = "catalog"
	PersonalSource = "personal"
)

// ListedExercise is a read projection. Catalog rows have no personal owner;
// source and ID together are their identity, including cross-source collisions.
type ListedExercise struct {
	Exercise
	Source string
}

func (e *ListedExercise) Validate() error {
	if e == nil || !validID(e.ID) || e.Revision < 1 || e.CreatedAt.IsZero() || e.CreatedAt.Year() < 1 || e.CreatedAt.Year() > 9999 || e.UpdatedAt.Before(e.CreatedAt) || e.UpdatedAt.Year() > 9999 {
		return trainingErrors.ErrInvalidExercise
	}
	if e.Source != CatalogSource && e.Source != PersonalSource || e.Source == CatalogSource && e.AccountID != uuid.Nil || e.Source == PersonalSource && !validID(e.AccountID) {
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

type ListFilter struct{ Query string }

func NormalizeListFilter(query string) (ListFilter, error) {
	if !validText(query, 0, 120) {
		return ListFilter{}, trainingErrors.ErrInvalidExercise
	}
	return ListFilter{Query: search.Normalize(query)}, nil
}

// Lists use raw UTF-8 name (PostgreSQL COLLATE "C"), then source, then UUID.
// Equivalent display names remain distinct. The cursor key uses the original
// name, at most 480 bytes, so Unicode search expansion cannot exceed its bound.
type ListPosition struct {
	Name, Source string
	ID           uuid.UUID
}

func (p ListPosition) Validate() error {
	if !validID(p.ID) || !validText(p.Name, 1, 120) || strings.TrimSpace(p.Name) == "" || p.Source != CatalogSource && p.Source != PersonalSource {
		return protocol.ErrCursorInvalid
	}
	return nil
}

func (p ListPosition) CursorPosition() protocol.CursorPosition {
	return protocol.CursorPosition{Key: p.Source + ":" + p.Name, ID: p.ID}
}

func PositionFromCursor(cursor protocol.CursorPosition) (*ListPosition, error) {
	source, name, ok := strings.Cut(cursor.Key, ":")
	p := &ListPosition{Name: name, Source: source, ID: cursor.ID}
	if !ok || cursor.Sequence != "" || p.Validate() != nil {
		return nil, protocol.ErrCursorInvalid
	}
	return p, nil
}

func (p ListPosition) After(previous ListPosition) bool {
	if p.Name != previous.Name {
		return p.Name > previous.Name
	}
	if p.Source != previous.Source {
		return p.Source > previous.Source
	}
	return p.ID.String() > previous.ID.String()
}

func (e *ListedExercise) Position() ListPosition {
	return ListPosition{Name: e.Name, Source: e.Source, ID: e.ID}
}
