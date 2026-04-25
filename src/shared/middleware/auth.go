package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

const (
	UserIDKey = "cuadra_user_id"
	GymIDKey  = "cuadra_gym_id"
	RoleKey   = "cuadra_role"
)

var errUnauthorized = errors.New("token de autenticación faltante o inválido")
var errForbidden = errors.New("no tienes permisos para esta acción")

// AuthMiddleware validates the bearer JWT and stashes user_id, gym_id, role
// into the Gin context. It also propagates IP and user-agent into the
// request context so downstream use cases can populate audit_log entries
// without each handler having to thread the request object.
func AuthMiddleware(tokens auth.TokenService) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" || !strings.HasPrefix(header, "Bearer ") {
			utils.ErrorResponse(c, http.StatusUnauthorized, errUnauthorized)
			c.Abort()
			return
		}
		token := strings.TrimPrefix(header, "Bearer ")
		claims, err := tokens.ValidateAccessToken(token)
		if err != nil {
			utils.ErrorResponse(c, http.StatusUnauthorized, errUnauthorized)
			c.Abort()
			return
		}
		c.Set(UserIDKey, claims.UserID)
		c.Set(GymIDKey, claims.GymID)
		c.Set(RoleKey, claims.Role)

		ctx := c.Request.Context()
		ctx = audit.WithIP(ctx, c.ClientIP())
		ctx = audit.WithUA(ctx, c.Request.UserAgent())
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// RequireOwner aborts the request with 403 unless the authenticated user has
// role=owner. Use it on routes that mutate gym-wide state (UC-005, UC-006-010).
func RequireOwner() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, _ := c.Get(RoleKey)
		if role != "owner" {
			utils.ErrorResponse(c, http.StatusForbidden, errForbidden)
			c.Abort()
			return
		}
		c.Next()
	}
}

func GetUserID(c *gin.Context) (uuid.UUID, bool) {
	v, ok := c.Get(UserIDKey)
	if !ok {
		return uuid.Nil, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

func GetGymID(c *gin.Context) (uuid.UUID, bool) {
	v, ok := c.Get(GymIDKey)
	if !ok {
		return uuid.Nil, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

func GetRole(c *gin.Context) (string, bool) {
	v, ok := c.Get(RoleKey)
	if !ok {
		return "", false
	}
	r, ok := v.(string)
	return r, ok
}
