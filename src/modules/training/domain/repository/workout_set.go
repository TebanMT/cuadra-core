package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type WorkoutSetRepository interface {
	CountIDs(shared.Transaction, uuid.UUID, uuid.UUID) (int64, error)
	GetByID(shared.Transaction, uuid.UUID, uuid.UUID) (*workout.Set, error)
	// False is a set-ID collision before any write. Any later error must roll
	// back the outer transaction, including the inserted set and journal.
	CreateAndAdvance(shared.Transaction, uuid.UUID, *workout.Set, *workout.Workout, int64) (bool, error)
}
