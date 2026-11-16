package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type RoutineRepository interface {
	// New references only. Historical snapshots use different availability rules.
	ReferencesAvailable(shared.Transaction, uuid.UUID, []routine.ExerciseRef) (bool, error)
	Create(shared.Transaction, uuid.UUID, *routine.Routine, *routine.DraftRecord) (bool, error)
	GetByID(shared.Transaction, uuid.UUID, uuid.UUID) (*routine.Routine, error)
	GetDraftByID(shared.Transaction, uuid.UUID, uuid.UUID) (*routine.DraftRecord, error)
}
