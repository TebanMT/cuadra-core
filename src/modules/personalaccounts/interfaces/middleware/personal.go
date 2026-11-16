//go:build server

// Package middleware authenticates the personal JWT only. Active account,
// device and session state must be checked again inside the use-case transaction.
package middleware

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type claimsKey struct{}

// GetClaims reads cryptographically verified IDs from the request context.
// It does not assert that the session/device/account is currently active.
func GetClaims(ctx context.Context) (security.AccessClaims, bool) {
	if ctx == nil {
		return security.AccessClaims{}, false
	}
	claims, ok := ctx.Value(claimsKey{}).(security.AccessClaims)
	return claims, ok
}

func NewPersonal(tokens security.AccessTokens, now func() time.Time) (gin.HandlerFunc, error) {
	if tokens == nil || now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		requestID := uuid.NewString()
		c.Header("X-Request-ID", requestID)
		encoded, ok := personalBearer(c.Request)
		if !ok {
			rejectPersonal(c, requestID)
			return
		}
		claims, err := tokens.Verify(encoded, now())
		if err != nil {
			rejectPersonal(c, requestID)
			return
		}
		// An authenticated actor must have an account-only scope even when
		// the later use case needs the auth pool. Preserve and reject existing
		// incompatible scopes rather than replacing the request context.
		ctx, err := shared.WithAuthenticatedPersonalAccount(c.Request.Context(), claims.AccountID)
		if err != nil {
			rejectPersonal(c, requestID)
			return
		}
		ctx = context.WithValue(ctx, claimsKey{}, claims)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, nil
}

func personalBearer(request *http.Request) (string, bool) {
	if request == nil || request.URL == nil {
		return "", false
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return "", false
	}
	for name := range query {
		if strings.EqualFold(name, "access_token") || strings.EqualFold(name, "token") || strings.EqualFold(name, "authorization") {
			return "", false
		}
	}
	var authorization []string
	// net/http canonicalizes wire headers. Iterating also rejects duplicates
	// inserted with different casing by an upstream in-process adapter.
	for name, values := range request.Header {
		if strings.EqualFold(name, "Authorization") {
			authorization = append(authorization, values...)
		}
	}
	if len(authorization) != 1 {
		return "", false
	}
	value := authorization[0]
	if !strings.HasPrefix(value, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(value, "Bearer ")
	if len(token) == 0 || len(token) > 512 || strings.Count(token, ".") != 2 {
		return "", false
	}
	for _, c := range token {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return "", false
		}
	}
	return token, true
}
func rejectPersonal(c *gin.Context, requestID string) {
	c.Header("WWW-Authenticate", "Bearer")
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": "AUTH_REQUIRED", "message": "Vuelve a iniciar el acceso.", "retryable": false, "request_id": requestID, "action": "authenticate", "fields": []any{}})
}
