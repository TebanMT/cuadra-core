//go:build server

package controllers

import (
	"context"
	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/gin-gonic/gin"
)

type completeRecoveryExecutor interface {
	Execute(context.Context, personalApp.CompleteRecoveryInput) (personalApp.AuthorizedResult, error)
}
type RecoveryController struct{ complete completeRecoveryExecutor }

func NewRecoveryController(complete completeRecoveryExecutor) (*RecoveryController, error) {
	if complete == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &RecoveryController{complete: complete}, nil
}
func (h *RecoveryController) Complete(c *gin.Context) {
	id := beginPersonalResponse(c)
	var body struct {
		AttemptID          string `json:"attempt_id"`
		RecoveryKeyID      string `json:"recovery_key_id"`
		RecoverySecret     string `json:"recovery_secret"`
		ConfirmPhoneChange *bool  `json:"confirm_phone_change"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/auth/recovery/complete", &body)
	if !ok {
		return
	}
	keyID, err := canonicalUUID(body.RecoveryKeyID)
	if err != nil || body.ConfirmPhoneChange == nil || !secretPattern.MatchString(body.RecoverySecret) {
		writeAuthError(c, id, personalErrors.ErrInvalidRecoveryKey)
		return
	}
	binding, ok := attemptBinding(c, id, body.AttemptID)
	if !ok {
		return
	}
	result, err := h.complete.Execute(c.Request.Context(), personalApp.CompleteRecoveryInput{Binding: binding, OperationID: op, RequestHash: hash, RecoveryKeyID: keyID, RecoverySecret: body.RecoverySecret, ConfirmPhoneChange: *body.ConfirmPhoneChange})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
