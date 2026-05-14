// Package category — Challenge categories are the buckets participants
// compete inside (Hombres / Mujeres on day 1; could grow to age brackets
// or weight classes). Intentionally a thin entity: uniqueness lives at
// the repository layer (DB unique index per challenge + LOWER(name)).
package category

import (
	"strings"
	"time"

	"github.com/google/uuid"

	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
)

type Category struct {
	ID          uuid.UUID
	GymID       uuid.UUID
	ChallengeID uuid.UUID
	Version     int
	Name        string
	SortOrder   int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// NewCategory constructs a category for the given challenge. Trims +
// validates the name; caller is responsible for the uniqueness check
// (repo-level via DB constraint).
func NewCategory(id, gymID, challengeID uuid.UUID, name string, sortOrder int, now time.Time) (*Category, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, challengeErrors.ErrCategoryNameRequired
	}
	return &Category{
		ID:          id,
		GymID:       gymID,
		ChallengeID: challengeID,
		Version:     1,
		Name:        trimmed,
		SortOrder:   sortOrder,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// Rename + Reorder are the only mutations; deletion is a repo concern
// (soft delete via deleted_at).
func (c *Category) Rename(name string, now time.Time) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return challengeErrors.ErrCategoryNameRequired
	}
	c.Name = trimmed
	c.Version++
	c.UpdatedAt = now
	return nil
}

func (c *Category) Reorder(sortOrder int, now time.Time) {
	c.SortOrder = sortOrder
	c.Version++
	c.UpdatedAt = now
}
