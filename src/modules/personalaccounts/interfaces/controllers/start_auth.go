//go:build server

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
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

type startExecutor interface {
	Execute(context.Context, personalApp.StartAuthInput) (*attempt.AuthAttempt, error)
}
type StartAuthController struct {
	start    startExecutor
	now      func() time.Time
	cooldown time.Duration
}

// The explicit cooldown is shared with the resend use case and mobile display;
// there is no hidden commercial default for sending OTP.
func NewStartAuthController(start startExecutor, now func() time.Time, cooldown time.Duration) (*StartAuthController, error) {
	if start == nil || now == nil || cooldown <= 0 || cooldown > time.Hour {
		return nil, personalErrors.ErrVerificationConfiguration
	}
	return &StartAuthController{start: start, now: now, cooldown: cooldown}, nil
}

type startBody struct {
	AttemptID string               `json:"attempt_id"`
	Phone     string               `json:"phone"`
	Country   string               `json:"country"`
	Channel   verification.Channel `json:"channel"`
	Purpose   attempt.Purpose      `json:"purpose"`
	Candidate struct {
		ID        string `json:"id"`
		Platform  string `json:"platform"`
		Label     string `json:"label"`
		PublicKey string `json:"public_key"`
	} `json:"candidate"`
	NewAccountID       string `json:"new_account_id"`
	ContinuationSecret string `json:"continuation_secret"`
}

var secretPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{32,512}$`)
var countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)

func canonicalUUID(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil || id.String() != value || id.Variant() != uuid.RFC4122 || id.Version() < 1 || id.Version() > 8 {
		return uuid.Nil, personalErrors.ErrInvalidAuthAttempt
	}
	return id, nil
}

func (h *StartAuthController) Start(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	requestID := uuid.NewString()
	c.Header("X-Request-ID", requestID)
	contentType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || contentType != "application/json" || c.Request.URL.RawQuery != "" {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	if len(c.Request.Header.Values("Idempotency-Key")) != 1 {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	operationID, err := canonicalUUID(c.GetHeader("Idempotency-Key"))
	if err != nil {
		writeAuthError(c, requestID, err)
		return
	}
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	raw, err := io.ReadAll(reader)
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			writeAuthFault(c, requestID, http.StatusRequestEntityTooLarge, "LIMIT_EXCEEDED", "La solicitud es demasiado grande.", "correct", false)
			return
		}
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	// Canonical hashing also rejects duplicate properties, invalid Unicode,
	// floating numbers and additional JSON values before any effects occur.
	hash, err := crypto.RequestHash("POST", "/auth/start", operationID, raw)
	if err != nil {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	var body startBody
	if err = decodeClosedJSON(raw, &body); err != nil {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	attemptID, err := canonicalUUID(body.AttemptID)
	if err != nil {
		writeAuthError(c, requestID, err)
		return
	}
	accountID, err := canonicalUUID(body.NewAccountID)
	if err != nil {
		writeAuthError(c, requestID, err)
		return
	}
	deviceID, err := canonicalUUID(body.Candidate.ID)
	if err != nil {
		writeAuthError(c, requestID, err)
		return
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(body.Candidate.PublicKey)
	if err != nil || len(key) != 32 || len(body.Candidate.PublicKey) != 43 || !countryPattern.MatchString(body.Country) || !secretPattern.MatchString(body.ContinuationSecret) {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	// Use the actual peer. Forwarded headers cannot choose another person's
	// budget. A trusted reverse proxy may supply its canonical origin through
	// a separately configured edge adapter in a later deployment integration.
	origin, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil || net.ParseIP(origin) == nil {
		writeAuthError(c, requestID, personalErrors.ErrInvalidAuthAttempt)
		return
	}
	a, err := h.start.Execute(c.Request.Context(), personalApp.StartAuthInput{OperationID: operationID, Binding: attempt.Binding{ID: attemptID, ContinuationHash: sha256.Sum256([]byte(body.ContinuationSecret))}, RequestHash: hash, ProposedAccountID: accountID, Candidate: attempt.Candidate{ID: deviceID, PublicKey: [32]byte(key), Platform: body.Candidate.Platform, Label: body.Candidate.Label}, Purpose: body.Purpose, DestinationE164: body.Phone, Channel: body.Channel, Origin: net.ParseIP(origin).String()})
	if err != nil {
		writeAuthError(c, requestID, err)
		return
	}
	c.JSON(http.StatusOK, dto.AuthAttemptFromDomain(a, h.now(), h.cooldown))
}

func writeAuthError(c *gin.Context, id string, err error) {
	switch {
	case errors.Is(err, personalErrors.ErrDeletionPolicyNotConfigured):
		writeAuthFault(c, id, 503, "POLICY_NOT_CONFIGURED", "La política de eliminación todavía no está disponible.", "wait", false)
	case errors.Is(err, personalErrors.ErrInvalidDeletion):
		writeAuthFault(c, id, 400, "VALIDATION_ERROR", "Revisa la solicitud de eliminación.", "correct", false)
	case errors.Is(err, personalErrors.ErrDeletionResultNotFound):
		writeAuthFault(c, id, 404, "NOT_FOUND", "No encontramos ese resultado.", "review", false)
	case errors.Is(err, personalErrors.ErrDeletionState):
		writeAuthFault(c, id, 403, "ACCOUNT_UNAVAILABLE", "La cuenta no está disponible para esta operación.", "authenticate", false)
	case errors.Is(err, personalErrors.ErrInvalidPhoneChange):
		writeAuthFault(c, id, 400, "VALIDATION_ERROR", "Revisa los datos del cambio de número.", "correct", false)
	case errors.Is(err, personalErrors.ErrRecoveryPhoneUnavailable):
		writeAuthFault(c, id, 409, "PHONE_UNAVAILABLE", "Ese número no está disponible. Conserva tu clave y usa otro número libre.", "contact_support", false)
	case errors.Is(err, personalErrors.ErrInvalidDeviceAuthorization):
		writeAuthFault(c, id, 400, "VALIDATION_ERROR", "Revisa la solicitud de autorización.", "correct", false)
	case errors.Is(err, personalErrors.ErrDeviceAuthorizationNotFound):
		writeAuthFault(c, id, 404, "NOT_FOUND", "No encontramos esa solicitud de autorización.", "review", false)
	case errors.Is(err, personalErrors.ErrDeviceAuthorizationExpired):
		writeAuthFault(c, id, 410, "ATTEMPT_EXPIRED", "La autorización venció. Vuelve a iniciar el acceso.", "authenticate", false)
	case errors.Is(err, personalErrors.ErrDeviceAuthorizationState):
		writeAuthFault(c, id, 409, "VERIFICATION_FAILED", "La autorización ya no está disponible. Consulta el intento.", "review", false)
	case errors.Is(err, personalErrors.ErrInvalidAuthAttempt), errors.Is(err, personalErrors.ErrVerificationInput), errors.Is(err, personalErrors.ErrInvalidRecoveryConfirmation), errors.Is(err, personalErrors.ErrInvalidRecoveryKey):
		writeAuthFault(c, id, 400, "VALIDATION_ERROR", "Revisa los datos de acceso.", "correct", false)
	case errors.Is(err, personalErrors.ErrVaultNotFound), errors.Is(err, personalErrors.ErrAuthAttemptNotFound), errors.Is(err, personalErrors.ErrReceiptNotFound), errors.Is(err, personalErrors.ErrRecoveryKeyNotFound), errors.Is(err, personalErrors.ErrAuthObjectNotFound):
		writeAuthFault(c, id, 404, "NOT_FOUND", "No encontramos ese intento.", "authenticate", false)
	case errors.Is(err, personalErrors.ErrIdempotencyMismatch), errors.Is(err, personalErrors.ErrAuthAttemptConflict):
		writeAuthFault(c, id, 409, "IDEMPOTENCY_MISMATCH", "La solicitud cambió. Vuelve a intentar con los mismos datos.", "review", false)
	case errors.Is(err, personalErrors.ErrAuthAttemptExpired):
		writeAuthFault(c, id, 410, "ATTEMPT_EXPIRED", "El intento venció. Solicita un código nuevo.", "authenticate", false)
	case errors.Is(err, personalErrors.ErrAuthAttemptCooldown), errors.Is(err, personalErrors.ErrBudgetExceeded), errors.Is(err, personalErrors.ErrBudgetBlocked), errors.Is(err, personalErrors.ErrAuthAttemptCheckLimit):
		writeAuthFault(c, id, 429, "RATE_LIMITED", "Espera antes de volver a intentarlo.", "wait", true)
	case errors.Is(err, personalErrors.ErrInvalidAccessToken), errors.Is(err, personalErrors.ErrInvalidRefresh), errors.Is(err, personalErrors.ErrInvalidLogout):
		writeAuthFault(c, id, 401, "AUTH_REQUIRED", "Vuelve a iniciar el acceso.", "authenticate", false)
	case errors.Is(err, personalErrors.ErrSessionRevoked):
		writeAuthFault(c, id, 401, "SESSION_REVOKED", "Se retiró el acceso de esta sesión.", "authenticate", false)
	case errors.Is(err, personalErrors.ErrAuthObjectUnavailable), errors.Is(err, personalErrors.ErrAccountUnavailable):
		writeAuthFault(c, id, 403, "ACCOUNT_UNAVAILABLE", "La cuenta no está disponible para esta operación.", "authenticate", false)

	case errors.Is(err, personalErrors.ErrVaultConflict):
		writeAuthFault(c, id, 409, "REVISION_CONFLICT", "Ese almacén ya tiene una clave protegida.", "review", false)
	case errors.Is(err, personalErrors.ErrVaultState):
		writeAuthFault(c, id, 409, "ENTITY_DELETED", "Ese almacén ya no está disponible.", "review", false)
	case errors.Is(err, personalErrors.ErrRecoveryKeyState):
		writeAuthFault(c, id, 409, "RECOVERY_UNAVAILABLE", "Esa clave de recuperación ya no está disponible.", "review", false)
	case errors.Is(err, personalErrors.ErrInvalidProof):
		writeAuthFault(c, id, 403, "INVALID_PROOF", "No pudimos comprobar el acceso de este dispositivo.", "authenticate", false)
	case errors.Is(err, personalErrors.ErrResultExpired):
		writeAuthFault(c, id, 410, "RESULT_EXPIRED", "El resultado protegido venció. Vuelve a iniciar el acceso.", "authenticate", false)
	case errors.Is(err, personalErrors.ErrAuthAttemptOperation), errors.Is(err, personalErrors.ErrAuthAttemptState):
		writeAuthFault(c, id, 409, "VERIFICATION_FAILED", "Consulta el intento y revisa el paso de acceso.", "review", false)
	default:
		writeAuthFault(c, id, 503, "TEMPORARY_UNAVAILABLE", "No pudimos confirmar el resultado. Consulta el intento antes de volver a enviar.", "retry", true)
	}
}
func writeAuthFault(c *gin.Context, id string, status int, code, message, action string, retryable bool) {
	c.JSON(status, gin.H{"code": code, "message": message, "retryable": retryable, "request_id": id, "action": action, "fields": []any{}})
}
