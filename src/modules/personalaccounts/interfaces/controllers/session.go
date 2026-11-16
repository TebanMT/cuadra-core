//go:build server

package controllers

import (
	"context"
	"encoding/base64"
	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/gin-gonic/gin"
)

type refreshExecutor interface {
	Execute(context.Context, personalApp.RefreshSessionInput) (personalApp.SessionTokens, error)
}
type logoutExecutor interface {
	Execute(context.Context, personalApp.LogoutInput) (personalApp.Acknowledgement, error)
}
type SessionController struct {
	refresh refreshExecutor
	logout  logoutExecutor
}

func NewSessionController(refresh refreshExecutor, logout logoutExecutor) (*SessionController, error) {
	if refresh == nil || logout == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &SessionController{refresh: refresh, logout: logout}, nil
}
func (h *SessionController) Refresh(c *gin.Context) {
	id := beginPersonalResponse(c)
	var body struct {
		RefreshToken    string `json:"refresh_token"`
		DeviceSignature string `json:"device_signature"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/auth/refresh", &body)
	if !ok {
		return
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(body.DeviceSignature)
	if !secretPattern.MatchString(body.RefreshToken) || err != nil || len(signature) != 64 || len(body.DeviceSignature) != 86 {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	result, err := h.refresh.Execute(c.Request.Context(), personalApp.RefreshSessionInput{OperationID: op, RequestHash: hash, RefreshToken: body.RefreshToken, DeviceSignature: body.DeviceSignature})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
func (h *SessionController) Logout(c *gin.Context) {
	id := beginPersonalResponse(c)
	var body struct {
		RevocationTicket string `json:"revocation_ticket"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/auth/logout", &body)
	if !ok {
		return
	}
	if !secretPattern.MatchString(body.RevocationTicket) {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	result, err := h.logout.Execute(c.Request.Context(), personalApp.LogoutInput{OperationID: op, RequestHash: hash, RevocationTicket: body.RevocationTicket})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
