//go:build server

package controllers

import (
	"context"
	"errors"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/gin-gonic/gin"
)

type listDevicesExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.ListDevicesInput) (personalApp.DeviceListResult, error)
}
type revokeDevicesExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.RevokeDevicesInput) (personalApp.Acknowledgement, error)
}
type DevicesController struct {
	list   listDevicesExecutor
	revoke revokeDevicesExecutor
}

func NewDevicesController(list listDevicesExecutor, revoke revokeDevicesExecutor) (*DevicesController, error) {
	if list == nil || revoke == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &DevicesController{list: list, revoke: revoke}, nil
}
func writeDeviceError(c *gin.Context, id string, err error) {
	switch {
	case errors.Is(err, protocol.ErrCursorExpired):
		writeAuthFault(c, id, 410, "CURSOR_EXPIRED", "Vuelve a cargar la lista de dispositivos desde el principio.", "retry", false)
	case errors.Is(err, protocol.ErrCursorEpochChanged):
		writeAuthFault(c, id, 410, "EPOCH_CHANGED", "La lista cambió. Vuelve a cargarla desde el principio.", "retry", false)
	case errors.Is(err, protocol.ErrCursorInvalid), errors.Is(err, protocol.ErrCommandInvalid), errors.Is(err, personalErrors.ErrInvalidDevice):
		writeAuthFault(c, id, 400, "VALIDATION_ERROR", "Revisa el dispositivo y la solicitud.", "correct", false)
	case errors.Is(err, personalErrors.ErrAuthObjectNotFound):
		writeAuthFault(c, id, 404, "NOT_FOUND", "No encontramos ese dispositivo.", "review", false)
	default:
		writeAuthError(c, id, err)
	}
}
func (h *DevicesController) List(c *gin.Context) {
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
	result, err := h.list.Execute(c.Request.Context(), actor, personalApp.ListDevicesInput{Cursor: cursor, Limit: limit})
	if err != nil {
		writeDeviceError(c, id, err)
		return
	}
	c.JSON(200, result)
}
func (h *DevicesController) RevokeOne(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	target, err := canonicalUUID(c.Param("id"))
	if err != nil {
		writeDeviceError(c, id, personalErrors.ErrInvalidDevice)
		return
	}
	var body struct{}
	op, hash, ok := decodePersonalMutation(c, id, "/me/devices/"+target.String()+"/revoke", &body)
	if !ok {
		return
	}
	proof, ok := readDeviceProof(c)
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidProof)
		return
	}
	result, err := h.revoke.Execute(c.Request.Context(), actor, personalApp.RevokeDevicesInput{OperationID: op, RequestHash: hash, ProofToken: proof, Target: device.RevocationTarget{DeviceID: target}})
	if err != nil {
		writeDeviceError(c, id, err)
		return
	}
	c.JSON(200, result)
}
func (h *DevicesController) RevokeScope(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	var body struct {
		Scope string `json:"scope"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/me/devices/revoke", &body)
	if !ok {
		return
	}
	target := device.RevocationTarget{Scope: body.Scope}
	if target.Validate() != nil {
		writeDeviceError(c, id, personalErrors.ErrInvalidDevice)
		return
	}
	proof, ok := readDeviceProof(c)
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidProof)
		return
	}
	result, err := h.revoke.Execute(c.Request.Context(), actor, personalApp.RevokeDevicesInput{OperationID: op, RequestHash: hash, ProofToken: proof, Target: target})
	if err != nil {
		writeDeviceError(c, id, err)
		return
	}
	c.JSON(200, result)
}
