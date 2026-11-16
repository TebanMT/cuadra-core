package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type ExerciseListRepository interface {
	// Caller holds the account mutex and has checked the active personal actor.
	// Returns active global catalog and own personal rows, with one lookahead.
	List(shared.Transaction, uuid.UUID, exercise.ListFilter, *exercise.ListPosition, int) ([]*exercise.ListedExercise, error)
}
