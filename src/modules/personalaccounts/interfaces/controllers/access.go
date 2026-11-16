//go:build server

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/dto"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type checkExecutor interface {
	Execute(context.Context, personalApp.CheckAccessInput) (personalApp.AccessResult, error)
}
type statusExecutor interface {
	Execute(context.Context, attempt.Binding) (personalApp.AccessResult, error)
}
type resendExecutor interface {
	Execute(context.Context, personalApp.ResendAuthInput) (*attempt.AuthAttempt, error)
}
type AccessController struct {
	check    checkExecutor
	status   statusExecutor
	resend   resendExecutor
	now      func() time.Time
	cooldown time.Duration
}

func NewAccessController(check checkExecutor, status statusExecutor, resend resendExecutor, now func() time.Time, cooldown time.Duration) (*AccessController, error) {
	if check == nil || status == nil || resend == nil || now == nil || cooldown <= 0 || cooldown > time.Hour {
		return nil, personalErrors.ErrVerificationConfiguration
	}
	return &AccessController{check: check, status: status, resend: resend, now: now, cooldown: cooldown}, nil
}
func beginPersonalResponse(c *gin.Context) string {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	id := uuid.NewString()
	c.Header("X-Request-ID", id)
	return id
}
func decodePersonalMutation(c *gin.Context, requestID, path string, body any) (uuid.UUID, [32]byte, bool) {
	return decodePersonalMutationBounded(c, requestID, path, body, 16*1024)
}
func decodePersonalMutationBounded(c *gin.Context, requestID, path string, body any, limit int64) (uuid.UUID, [32]byte, bool) {
	var zero [32]byte
	content, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || content != "application/json" || len(c.Request.Header.Values("Idempotency-Key")) != 1 || c.Request.URL.RawQuery != "" {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return uuid.Nil, zero, false
	}
	id, err := canonicalUUID(c.GetHeader("Idempotency-Key"))
	if err != nil {
		writeAuthError(c, requestID, err)
		return uuid.Nil, zero, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, limit))
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			writeAuthFault(c, requestID, 413, "LIMIT_EXCEEDED", "La solicitud es demasiado grande.", "correct", false)
		} else {
			writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		}
		return uuid.Nil, zero, false
	}
	hash, err := crypto.RequestHash("POST", path, id, raw)
	if err != nil {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return uuid.Nil, zero, false
	}
	if err = decodeClosedJSON(raw, body); err != nil {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return uuid.Nil, zero, false
	}
	return id, hash, true
}
func attemptBinding(c *gin.Context, requestID, id string) (attempt.Binding, bool) {
	parsed, err := canonicalUUID(id)
	if err != nil {
		writeAuthError(c, requestID, err)
		return attempt.Binding{}, false
	}
	secret := c.GetHeader("X-Attempt-Secret")
	if len(c.Request.Header.Values("X-Attempt-Secret")) != 1 || !secretPattern.MatchString(secret) {
		writeAuthFault(c, requestID, 401, "AUTH_REQUIRED", "Vuelve a iniciar el acceso.", "authenticate", false)
		return attempt.Binding{}, false
	}
	return attempt.Binding{ID: parsed, ContinuationHash: sha256.Sum256([]byte(secret))}, true
}
func personalOrigin(c *gin.Context, requestID string) (string, bool) {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return "", false
	}
	return ip.String(), true
}
func (h *AccessController) Check(c *gin.Context) {
	requestID := beginPersonalResponse(c)
	var body struct {
		AttemptID       string          `json:"attempt_id"`
		Code            string          `json:"code"`
		DeviceSignature json.RawMessage `json:"device_signature"`
	}
	op, hash, ok := decodePersonalMutation(c, requestID, "/auth/check", &body)
	if !ok {
		return
	}
	binding, ok := attemptBinding(c, requestID, body.AttemptID)
	if !ok {
		return
	}
	signature := ""
	if len(body.DeviceSignature) > 0 {
		if json.Unmarshal(body.DeviceSignature, &signature) != nil {
			writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
			return
		}
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(signature)
		if err != nil || len(decoded) != 64 || len(signature) != 86 {
			writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
			return
		}
	}
	if len(body.Code) < 4 || len(body.Code) > 10 {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	for _, digit := range body.Code {
		if digit < '0' || digit > '9' {
			writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
			return
		}
	}
	origin, ok := personalOrigin(c, requestID)
	if !ok {
		return
	}
	result, err := h.check.Execute(c.Request.Context(), personalApp.CheckAccessInput{CheckPhoneVerificationInput: personalApp.CheckPhoneVerificationInput{OperationID: op, Binding: binding, RequestHash: hash, Code: body.Code, Origin: origin}, DeviceSignature: signature})
	if err != nil {
		writeAuthError(c, requestID, err)
		return
	}
	h.writeResult(c, result)
}
func (h *AccessController) Get(c *gin.Context) {
	requestID := beginPersonalResponse(c)
	if c.Request.URL.RawQuery != "" {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	binding, ok := attemptBinding(c, requestID, c.Param("id"))
	if !ok {
		return
	}
	result, err := h.status.Execute(c.Request.Context(), binding)
	if err != nil {
		writeAuthError(c, requestID, err)
		return
	}
	h.writeResult(c, result)
}
func (h *AccessController) Resend(c *gin.Context) {
	requestID := beginPersonalResponse(c)
	binding, ok := attemptBinding(c, requestID, c.Param("id"))
	if !ok {
		return
	}
	var body struct {
		Channel verification.Channel `json:"channel"`
	}
	op, hash, ok := decodePersonalMutation(c, requestID, "/auth/attempts/"+binding.ID.String()+"/resend", &body)
	if !ok {
		return
	}
	if body.Channel != verification.SMS && body.Channel != verification.WhatsApp {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	origin, ok := personalOrigin(c, requestID)
	if !ok {
		return
	}
	a, err := h.resend.Execute(c.Request.Context(), personalApp.ResendAuthInput{OperationID: op, Binding: binding, RequestHash: hash, Channel: body.Channel, Origin: origin})
	if err != nil {
		writeAuthError(c, requestID, err)
		return
	}
	c.JSON(200, dto.AuthAttemptFromDomain(a, h.now(), h.cooldown))
}
func (h *AccessController) writeResult(c *gin.Context, result personalApp.AccessResult) {
	if result.Authorized != nil {
		c.JSON(200, result.Authorized)
		return
	}
	a := dto.AuthAttemptFromDomain(result.Attempt, h.now(), h.cooldown)
	switch result.StateOverride {
	case "":
	case personalApp.AuthorizationPending, personalApp.AuthorizationDenied, personalApp.AuthorizationExpired, personalApp.AuthorizationCancelled:
		a.State = string(result.StateOverride)
		a.ResendAfterSeconds = 0
	default:
		writeAuthFault(c, c.Writer.Header().Get("X-Request-ID"), 503, "TEMPORARY_UNAVAILABLE", "No pudimos consultar el acceso. Conservamos el intento.", "retry", true)
		return
	}
	status := "pending"
	if result.AuthorizationRequired && a.State == "verified" {
		status = "authorization_required"
		a.State = "authorization_required"
	}
	c.JSON(200, gin.H{"status": status, "attempt": a})
}
