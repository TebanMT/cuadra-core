//go:build server

package controllers

import (
	"context"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
)

type startDeviceAuthorizationExecutor interface {
	Execute(context.Context, personalApp.StartDeviceAuthorizationInput) (personalApp.DeviceAuthorizationResult, error)
}
type listDeviceAuthorizationsExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.ListDeviceAuthorizationsInput) (personalApp.DeviceAuthorizationListResult, error)
}
type decideDeviceAuthorizationExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.DecideDeviceAuthorizationInput) (personalApp.Acknowledgement, error)
}
type DeviceAuthorizationController struct {
	start  startDeviceAuthorizationExecutor
	list   listDeviceAuthorizationsExecutor
	decide decideDeviceAuthorizationExecutor
}

func NewDeviceAuthorizationController(start startDeviceAuthorizationExecutor, list listDeviceAuthorizationsExecutor, decide decideDeviceAuthorizationExecutor) (*DeviceAuthorizationController, error) {
	if start == nil || list == nil || decide == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &DeviceAuthorizationController{start: start, list: list, decide: decide}, nil
}
func (h *DeviceAuthorizationController) Start(c *gin.Context) {
	id := beginPersonalResponse(c)
	var body struct {
		AttemptID string `json:"attempt_id"`
		ID        string `json:"id"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/auth/device-authorizations", &body)
	if !ok {
		return
	}
	binding, ok := attemptBinding(c, id, body.AttemptID)
	if !ok {
		return
	}
	authorizationID, err := canonicalUUID(body.ID)
	if err != nil {
		writeAuthError(c, id, personalErrors.ErrInvalidDeviceAuthorization)
		return
	}
	result, err := h.start.Execute(c.Request.Context(), personalApp.StartDeviceAuthorizationInput{Binding: binding, AuthorizationID: authorizationID, OperationID: op, RequestHash: hash})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
func (h *DeviceAuthorizationController) List(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	cursor, limit, err := personalPageQuery(c.Request.URL.RawQuery)
	if err != nil {
		writeDeviceError(c, id, err)
		return
	}
	result, err := h.list.Execute(c.Request.Context(), actor, personalApp.ListDeviceAuthorizationsInput{Cursor: cursor, Limit: limit})
	if err != nil {
		writeDeviceError(c, id, err)
		return
	}
	c.JSON(200, result)
}
func (h *DeviceAuthorizationController) Decide(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	authorizationID, err := canonicalUUID(c.Param("id"))
	if err != nil {
		writeAuthError(c, id, personalErrors.ErrInvalidDeviceAuthorization)
		return
	}
	var body struct {
		Decision deviceauthorization.Decision `json:"decision"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/me/device-authorizations/"+authorizationID.String()+"/decision", &body)
	if !ok {
		return
	}
	if body.Decision != deviceauthorization.Approve && body.Decision != deviceauthorization.Deny {
		writeAuthError(c, id, personalErrors.ErrInvalidDeviceAuthorization)
		return
	}
	proof, ok := readDeviceProof(c)
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidProof)
		return
	}
	result, err := h.decide.Execute(c.Request.Context(), actor, personalApp.DecideDeviceAuthorizationInput{AuthorizationID: authorizationID, OperationID: op, RequestHash: hash, Decision: body.Decision, ProofToken: proof})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
