//go:build server

package controllers

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/dto"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
)

type startPhoneChangeExecutor interface {
	Execute(context.Context, personalApp.StartPhoneChangeInput) (*attempt.AuthAttempt, error)
}
type completePhoneChangeExecutor interface {
	Execute(context.Context, personalApp.CompletePhoneChangeInput) (personalApp.SessionTokens, error)
}
type PhoneChangeController struct {
	start    startPhoneChangeExecutor
	complete completePhoneChangeExecutor
	now      func() time.Time
	cooldown time.Duration
}

func NewPhoneChangeController(start startPhoneChangeExecutor, complete completePhoneChangeExecutor, now func() time.Time, cooldown time.Duration) (*PhoneChangeController, error) {
	if start == nil || complete == nil || now == nil || cooldown <= 0 || cooldown > time.Hour {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &PhoneChangeController{start, complete, now, cooldown}, nil
}

// The personal phone-change contract puts the continuation secret in the body.
// Reject the anonymous attempt header rather than accepting two authorities.
func personalPhoneBinding(c *gin.Context, requestID, attemptID, secret string) (attempt.Binding, bool) {
	id, err := canonicalUUID(attemptID)
	if err != nil || !secretPattern.MatchString(secret) || len(c.Request.Header.Values("X-Attempt-Secret")) != 0 {
		writeAuthError(c, requestID, personalErrors.ErrInvalidPhoneChange)
		return attempt.Binding{}, false
	}
	return attempt.Binding{ID: id, ContinuationHash: sha256.Sum256([]byte(secret))}, true
}
func (h *PhoneChangeController) Start(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	var body struct {
		AttemptID          string               `json:"attempt_id"`
		Phone              string               `json:"phone"`
		Country            string               `json:"country"`
		Channel            verification.Channel `json:"channel"`
		ContinuationSecret string               `json:"continuation_secret"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/me/phone-change/start", &body)
	if !ok {
		return
	}
	if !countryPattern.MatchString(body.Country) {
		writeAuthError(c, id, personalErrors.ErrInvalidPhoneChange)
		return
	}
	binding, ok := personalPhoneBinding(c, id, body.AttemptID, body.ContinuationSecret)
	if !ok {
		return
	}
	proof, ok := readDeviceProof(c)
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidProof)
		return
	}
	origin, ok := personalOrigin(c, id)
	if !ok {
		return
	}
	result, err := h.start.Execute(c.Request.Context(), personalApp.StartPhoneChangeInput{Actor: actor, OperationID: op, RequestHash: hash, Binding: binding, DestinationE164: body.Phone, Channel: body.Channel, Origin: origin, ProofToken: proof})
	if err != nil {
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, dto.AuthAttemptFromDomain(result, h.now(), h.cooldown))
}
func (h *PhoneChangeController) Complete(c *gin.Context) {
	id := beginPersonalResponse(c)
	actor, ok := personalMiddleware.GetClaims(c.Request.Context())
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidAccessToken)
		return
	}
	var body struct {
		AttemptID          string `json:"attempt_id"`
		Code               string `json:"code"`
		ContinuationSecret string `json:"continuation_secret"`
	}
	op, hash, ok := decodePersonalMutation(c, id, "/me/phone-change/complete", &body)
	if !ok {
		return
	}
	binding, ok := personalPhoneBinding(c, id, body.AttemptID, body.ContinuationSecret)
	if !ok {
		return
	}
	proof, ok := readDeviceProof(c)
	if !ok {
		writeAuthError(c, id, personalErrors.ErrInvalidProof)
		return
	}
	origin, ok := personalOrigin(c, id)
	if !ok {
		return
	}
	result, err := h.complete.Execute(c.Request.Context(), personalApp.CompletePhoneChangeInput{Actor: actor, OperationID: op, RequestHash: hash, Binding: binding, Code: body.Code, Origin: origin, ProofToken: proof})
	if err != nil {
		if errors.Is(err, personalErrors.ErrVerificationRejected) {
			writeAuthFault(c, id, 409, "VERIFICATION_FAILED", "El código no se pudo verificar. Revisa el código antes de confirmar el cambio.", "correct", false)
			return
		}
		writeAuthError(c, id, err)
		return
	}
	c.JSON(200, result)
}
