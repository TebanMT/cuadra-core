package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type WorkoutRepository interface {
	// False means a global identity collision. Every aggregate insert is undone
	// within a narrow savepoint, leaving the outer receipt transaction usable.
	Create(shared.Transaction, uuid.UUID, *workout.Workout) (bool, error)
	GetByID(shared.Transaction, uuid.UUID, uuid.UUID) (*workout.Workout, error)
}
type RoutineVersionReader interface {
	GetVersion(shared.Transaction, uuid.UUID, uuid.UUID) (*routine.Version, error)
}

// Valid must use an IANA catalogue without a machine-local or UTC fallback.
type TimezoneValidator interface{ Valid(string) bool }
