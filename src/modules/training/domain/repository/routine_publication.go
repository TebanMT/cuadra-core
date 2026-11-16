package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type RoutinePublicationRepository interface {
	ResolveMaterial(shared.Transaction, uuid.UUID, *routine.DraftRecord) (routine.PublicationMaterial, error)
	// False is an identity collision: the narrow SQL savepoint has undone every
	// publication effect, allowing a terminal receipt in the caller's same tx.
	Publish(shared.Transaction, uuid.UUID, *routine.Publication) (bool, error)
	GetVersion(shared.Transaction, uuid.UUID, uuid.UUID) (*routine.Version, error)
}
