package app

import (
	"errors"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

// Resolve only committed receipt evidence. A missing dependency is not a
// terminal operation and must not be turned into a new current-revision write.
func resolveTrainingBase(tx shared.Transaction, commands protocolRepository.Commands, ownerID, operationID uuid.UUID, base protocol.Base, entityType, entityID string) (int64, *protocol.OperationResult, error) {
	if base.Revision != nil {
		revision, err := protocol.ParseRevision(*base.Revision)
		return revision, nil, err
	}
	parent, err := commands.GetReceipt(tx, ownerID, uuid.MustParse(*base.AfterOperationID))
	if errors.Is(err, protocol.ErrOperationMissing) {
		result := protocol.Failure(operationID, "dependency_pending", "DEPENDENCY_PENDING", "Falta confirmar el cambio anterior.", "wait", true)
		return 0, &result, nil
	}
	if err != nil {
		return 0, nil, err
	}
	if parent.Result.Outcome != "accepted" && parent.Result.Outcome != "accepted_with_review" {
		result := protocol.Failure(operationID, "dependency_blocked", "DEPENDENCY_BLOCKED", "Revisa el cambio anterior antes de continuar.", "review", false)
		return 0, &result, nil
	}
	var revision int64
	matches := 0
	for _, affected := range parent.Result.Affected {
		if affected.EntityType == entityType && affected.EntityID == entityID {
			matches++
			revision, err = protocol.ParseRevision(affected.Revision)
		}
	}
	if matches != 1 || err != nil || revision < 1 {
		result := protocol.Failure(operationID, "rejected", "VALIDATION_ERROR", "El cambio anterior no corresponde a esta revisión.", "correct", false)
		return 0, &result, nil
	}
	return revision, nil, nil
}
