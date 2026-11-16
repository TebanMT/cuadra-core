package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// The caller owns its transaction and authorizes the active actor. Create never
// updates an existing ID, including IDs hidden by another account's RLS.
type PersonalExerciseRepository interface {
	Create(shared.Transaction, uuid.UUID, *exercise.Exercise) (bool, error)
	GetByID(shared.Transaction, uuid.UUID, uuid.UUID) (*exercise.Exercise, error)
}
