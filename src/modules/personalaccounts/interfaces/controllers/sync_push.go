//go:build server

package controllers

import (
	"context"
	"encoding/json"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/gin-gonic/gin"
)

type pushWorkoutsExecutor interface {
	Execute(context.Context, security.AccessClaims, []workout.OfflineCommand) ([]protocol.OperationResult, error)
}

type PushController struct{ push pushWorkoutsExecutor }

func NewPushController(push pushWorkoutsExecutor) (*PushController, error) {
	if push == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &PushController{push: push}, nil
}

func (h *PushController) Execute(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	var envelope struct {
		ProtocolVersion *int              `json:"protocol_version"`
		Operations      []json.RawMessage `json:"operations"`
	}
	// The header identifies a request, not a global batch transaction. Each
	// operation carries its own immutable ID and is independently deduplicated.
	if _, _, ok := decodePersonalMutationBounded(c, id, "/sync/push", &envelope, 2097152); !ok {
		return
	}
	if envelope.ProtocolVersion == nil || envelope.Operations == nil || len(envelope.Operations) < 1 || len(envelope.Operations) > 50 {
		writeCommandError(c, id, protocol.ErrCommandInvalid)
		return
	}
	if *envelope.ProtocolVersion != 1 {
		writeAuthFault(c, id, 426, "UPGRADE_REQUIRED", "Actualiza la app para sincronizar. Tus capturas se conservan en este teléfono.", "update_app", false)
		return
	}
	commands := make([]workout.OfflineCommand, len(envelope.Operations))
	// Decode the ENTIRE closed supported union before executing anything.
	// Future offline variants remain unavailable until their producer is mounted.
	for i, raw := range envelope.Operations {
		// The union's custom decoder validates exact keys recursively. Reflective
		// struct-shape validation cannot represent its wire discriminator.
		if json.Unmarshal(raw, &commands[i]) != nil || commands[i].ValidateEnvelope() != nil {
			writeCommandError(c, id, protocol.ErrCommandInvalid)
			return
		}
	}
	results, err := h.push.Execute(c.Request.Context(), actor, commands)
	if err != nil {
		writeCommandError(c, id, err)
		return
	}
	c.JSON(200, struct {
		ProtocolVersion int                        `json:"protocol_version"`
		Results         []protocol.OperationResult `json:"results"`
	}{1, results})
}
