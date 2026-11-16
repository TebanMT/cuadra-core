//go:build server

package controllers

import (
	"context"
	"strings"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type personalAuthResultExecutor interface {
	Execute(context.Context, security.AccessClaims, uuid.UUID) (personalApp.PersonalAuthResult, error)
}
type deletionResultExecutor interface {
	Execute(context.Context, personalApp.GetDeletionResultInput) (personalApp.Acknowledgement, error)
}
type AuthResultController struct {
	get      personalAuthResultExecutor
	deletion deletionResultExecutor
}

func NewAuthResultController(get personalAuthResultExecutor) (*AuthResultController, error) {
	if get == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &AuthResultController{get: get}, nil
}
func NewAuthResultControllerWithDeletion(get personalAuthResultExecutor, deletion deletionResultExecutor) (*AuthResultController, error) {
	if deletion == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	h, err := NewAuthResultController(get)
	if err != nil {
		return nil, err
	}
	h.deletion = deletion
	return h, nil
}
func (h *AuthResultController) Get(c *gin.Context) {
	id := beginPersonalResponse(c)
	op, err := canonicalUUID(c.Param("id"))
	if err != nil || c.Request.URL.RawQuery != "" || c.Request.ContentLength != 0 {
		writeAuthError(c, id, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	if secret, limited := personalMiddleware.GetResultSecret(c.Request.Context()); limited {
		if _, authenticated := personalMiddleware.GetClaims(c.Request.Context()); authenticated || h.deletion == nil {
			writeAuthError(c, id, personalErrors.ErrInvalidAuthAttempt)
			return
		}
		result, err := h.deletion.Execute(c.Request.Context(), personalApp.GetDeletionResultInput{OperationID: op, ResultSecret: secret})
		if err != nil {
			writeAuthError(c, id, err)
			return
		}
		c.JSON(200, result)
		return
	}
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	// The personal branch never accepts a second result/attempt authority.
	for name := range c.Request.Header {
		if strings.EqualFold(name, "X-Attempt-Secret") || strings.EqualFold(name, "X-Result-Secret") {
			writeAuthError(c, id, personalErrors.ErrInvalidAuthAttempt)
			return
		}
	}
	result, err := h.get.Execute(c.Request.Context(), actor, op)
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	if result.Session != nil && result.RecoveryKit == nil && result.Acknowledgement == nil {
		c.JSON(200, result.Session)
		return
	}
	if result.RecoveryKit != nil && result.Session == nil && result.Acknowledgement == nil {
		c.JSON(200, result.RecoveryKit)
		return
	}
	if result.Acknowledgement != nil && result.Session == nil && result.RecoveryKit == nil {
		c.JSON(200, result.Acknowledgement)
		return
	}
	writeAuthError(c, id, personalErrors.ErrInvalidEnvelope)
}
