//go:build server

package middleware

import (
	"context"
	"encoding/base64"
	"strings"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type resultSecretKey struct{}

// GetResultSecret is a limited capability, never personal account authority.
func GetResultSecret(ctx context.Context) (string, bool) {
	secret, ok := ctx.Value(resultSecretKey{}).(string)
	return secret, ok
}

// NewResultAccess belongs exclusively on GET /auth/results/:id. A result
// secret and a personal JWT are mutually exclusive. The limited branch keeps
// the request anonymous; its use case resolves and returns only a deletion ack.
func NewResultAccess(authenticate gin.HandlerFunc) (gin.HandlerFunc, error) {
	if authenticate == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return func(c *gin.Context) {
		var values []string
		var resultPresent, authorizationPresent, attemptPresent bool
		for name, header := range c.Request.Header {
			switch {
			case strings.EqualFold(name, "X-Result-Secret"):
				resultPresent = true
				values = append(values, header...)
			case strings.EqualFold(name, "Authorization"):
				authorizationPresent = true
			case strings.EqualFold(name, "X-Attempt-Secret"):
				attemptPresent = true
			}
		}
		if attemptPresent || resultPresent && authorizationPresent {
			rejectResultAuthority(c)
			return
		}
		if !resultPresent {
			authenticate(c)
			return
		}
		if len(values) != 1 || len(values[0]) != 43 {
			rejectResultAuthority(c)
			return
		}
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(values[0])
		defer clear(decoded)
		_, hasClaims := GetClaims(c.Request.Context())
		if err != nil || len(decoded) != 32 || hasClaims || shared.ValidatePersonalAnonymousScope(c.Request.Context()) != nil {
			rejectResultAuthority(c)
			return
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), resultSecretKey{}, values[0]))
		c.Next()
	}, nil
}

func rejectResultAuthority(c *gin.Context) {
	id := uuid.NewString()
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Header("X-Request-ID", id)
	c.AbortWithStatusJSON(400, gin.H{"code": "VALIDATION_ERROR", "message": "Usa un solo medio para consultar el resultado.", "retryable": false, "request_id": id, "action": "correct", "fields": []any{}})
}
