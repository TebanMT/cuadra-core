//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// ExerciseListModel is the UNION ALL projection, never a writable GORM model.
type ExerciseListModel struct {
	ID                    uuid.UUID
	AccountID             *uuid.UUID
	Source                string
	Name                  string
	LoadMode              string
	Notes                 string
	Revision              int64
	CreatedAt, UpdatedAt  time.Time
	ArchivedAt, DeletedAt *time.Time
}
