package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type RoutineListRepository interface {
	// Caller holds the account mutex and has checked the current actor. Limit
	// includes the lookahead item; deleted roots never enter a library list.
	List(shared.Transaction, uuid.UUID, routine.ListFilter, *routine.ListPosition, int) ([]*routine.Routine, error)
}
