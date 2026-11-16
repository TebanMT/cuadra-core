//go:build server

package controllers

import (
	"context"
	"errors"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/gin-gonic/gin"
)

type updateProfileExecutor interface {
	Execute(context.Context, security.AccessClaims, account.ProfileCommand) (protocol.OperationResult, error)
}
type ProfileCommandController struct{ update updateProfileExecutor }

func NewProfileCommandController(update updateProfileExecutor) (*ProfileCommandController, error) {
	if update == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &ProfileCommandController{update: update}, nil
}
func (h *ProfileCommandController) Execute(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	var command account.ProfileCommand
	op, _, ok := decodePersonalMutation(c, id, "/commands", &command)
	if !ok {
		return
	}
	if command.OperationID != op.String() || command.ValidateEnvelope() != nil || account.ValidateProfile(command.Payload.DisplayName, command.Payload.WeightUnit) != nil {
		writeAuthFault(c, id, 400, "VALIDATION_ERROR", "Revisa los datos del perfil.", "correct", false)
		return
	}
	result, err := h.update.Execute(c.Request.Context(), actor, command)
	if err != nil {
		switch {
		case errors.Is(err, protocol.ErrOperationMismatch):
			writeAuthFault(c, id, 409, "IDEMPOTENCY_MISMATCH", "La operación cambió. Conserva el cambio original para reintentar.", "review", false)
		case errors.Is(err, protocol.ErrCommandActor):
			writeAuthError(c, id, personalErrors.ErrSessionRevoked)
		case errors.Is(err, protocol.ErrCommandInvalid), errors.Is(err, protocol.ErrCommandOrigin):
			writeAuthFault(c, id, 400, "VALIDATION_ERROR", "Revisa los datos del cambio.", "correct", false)
		default:
			writeAuthFault(c, id, 503, "TEMPORARY_UNAVAILABLE", "No pudimos confirmar el cambio. Conservamos la operación para reintentar.", "retry", true)
		}
		return
	}
	c.JSON(200, result)
}
