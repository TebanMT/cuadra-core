//go:build server

package controllers

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
)

type deviceAuthorizationResultExecutor interface {
	Execute(context.Context, security.AccessClaims, personalApp.GetDeviceAuthorizationDecisionResultInput) (personalApp.DeviceAuthorizationDecisionResult, error)
}

type DeviceAuthorizationResultController struct {
	get deviceAuthorizationResultExecutor
}

func NewDeviceAuthorizationResultController(get deviceAuthorizationResultExecutor) (*DeviceAuthorizationResultController, error) {
	if get == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &DeviceAuthorizationResultController{get: get}, nil
}

// The query carries only the original effect's identity. It is not a proof,
// continuation capability, or authority to execute that effect again.
func (h *DeviceAuthorizationResultController) Get(c *gin.Context) {
	requestID := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAccessToken)
		return
	}
	invalid := func() { writeAuthError(c, requestID, personalErrors.ErrInvalidDeviceAuthorization) }
	if c.Request.ContentLength != 0 {
		invalid()
		return
	}
	for name := range c.Request.Header {
		if strings.EqualFold(name, "X-Attempt-Secret") || strings.EqualFold(name, "X-Result-Secret") || strings.EqualFold(name, "X-Device-Proof") || strings.EqualFold(name, "Idempotency-Key") {
			invalid()
			return
		}
	}
	q, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil || len(q) != 3 || len(q["original_session_id"]) != 1 || len(q["original_device_id"]) != 1 || len(q["decision"]) != 1 {
		invalid()
		return
	}
	authorizationID, err := canonicalUUID(c.Param("id"))
	if err != nil {
		invalid()
		return
	}
	operationID, err := canonicalUUID(c.Param("operation_id"))
	if err != nil {
		invalid()
		return
	}
	sessionID, err := canonicalUUID(q.Get("original_session_id"))
	if err != nil {
		invalid()
		return
	}
	deviceID, err := canonicalUUID(q.Get("original_device_id"))
	if err != nil {
		invalid()
		return
	}
	decision := deviceauthorization.Decision(q.Get("decision"))
	if decision != deviceauthorization.Approve && decision != deviceauthorization.Deny {
		invalid()
		return
	}
	body, err := json.Marshal(struct {
		Decision deviceauthorization.Decision `json:"decision"`
	}{decision})
	if err != nil {
		invalid()
		return
	}
	hash, err := crypto.RequestHash("POST", "/me/device-authorizations/"+authorizationID.String()+"/decision", operationID, body)
	if err != nil {
		invalid()
		return
	}
	result, err := h.get.Execute(c.Request.Context(), actor, personalApp.GetDeviceAuthorizationDecisionResultInput{
		AuthorizationID: authorizationID, OperationID: operationID, OriginalSessionID: sessionID, OriginalDeviceID: deviceID, Decision: decision, RequestHash: hash,
	})
	if err != nil {
		writeAuthError(c, requestID, err)
		return
	}
	c.JSON(200, result)
}
