//go:build server

package controllers

import (
	"context"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
)

type meExecutor interface {
	Execute(context.Context, security.AccessClaims) (personalApp.MeResult, error)
}
type confirmRecoveryExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.ConfirmRecoveryKeyInput) (personalApp.Acknowledgement, error)
}
type MeController struct {
	me      meExecutor
	confirm confirmRecoveryExecutor
}

func NewMeController(me meExecutor, confirm confirmRecoveryExecutor) (*MeController, error) {
	if me == nil || confirm == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &MeController{me: me, confirm: confirm}, nil
}
func (h *MeController) Get(c *gin.Context) {
	id := beginPersonalResponse(c)
	claims, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAuthError(c, id, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	result, err := h.me.Execute(c.Request.Context(), claims)
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
func (h *MeController) ConfirmRecovery(c *gin.Context) {
	id := beginPersonalResponse(c)
	claims, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	var body struct {
		RecoveryKeyID string `json:"recovery_key_id"`
		Saved         bool   `json:"saved"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/me/recovery-key/confirm", &body)
	if !ok {
		return
	}
	keyID, err := canonicalUUID(body.RecoveryKeyID)
	if err != nil || !body.Saved {
		writeAuthError(c, id, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	result, err := h.confirm.Execute(c.Request.Context(), claims, personalApp.ConfirmRecoveryKeyInput{OperationID: op, RequestHash: hash, RecoveryKeyID: keyID, Saved: body.Saved})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
