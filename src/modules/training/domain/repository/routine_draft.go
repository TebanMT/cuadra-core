package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// The same account transaction owns validation, CAS and protocol metadata.
// Historical references are matched to persisted occurrence IDs, never merely
// inferred from a supplied exercise ID or from a foreign routine version.
type RoutineDraftRepository interface {
	ReferencesAvailableForSave(shared.Transaction, uuid.UUID, uuid.UUID, routine.Draft, *uuid.UUID) (bool, error)
	Save(shared.Transaction, uuid.UUID, *routine.DraftRecord, int64) (bool, error)
}
