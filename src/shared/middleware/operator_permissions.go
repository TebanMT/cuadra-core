package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// operatorRoutePermissions maps the routes an operator can only use with a
// permission ("METHOD /full/path" as Gin registers it). Permissions that
// depend on the request content (a discount, a partial payment, a cash
// outflow, a member's new status) are checked by the use case with
// sharedDomain.RequirePermission instead.
var operatorRoutePermissions = map[string]sharedDomain.Permission{
	"POST /api/v1/products":                   sharedDomain.PermProducts,
	"PATCH /api/v1/products/:id":              sharedDomain.PermProducts,
	"DELETE /api/v1/products/:id":             sharedDomain.PermProducts,
	"POST /api/v1/products/:id/reactivate":    sharedDomain.PermProducts,
	"POST /api/v1/products/:id/adjust-stock":  sharedDomain.PermInventory,
	"POST /api/v1/product-counts":             sharedDomain.PermInventory,
	"POST /api/v1/checkins/override":          sharedDomain.PermCheckinOverride,
	"POST /api/v1/cash-sessions/:id/withdraw": sharedDomain.PermCashOut,
}

// EnforceOperatorPermissions resolves what the authenticated user may do on
// every request and stores it in the request context
// (sharedDomain.WithGrants), so use cases can check content-dependent
// permissions. Routes listed in operatorRoutePermissions are rejected here
// with 403 when the operator lacks the permission.
//
// It goes on the engine, before any route is registered. The owner is
// resolved from the token without a query; an operator is read from the
// users table on each request, so a change the owner makes applies on the
// next one. If the operator can't be read, they get no permissions (fail
// closed) and only the gated actions are refused.
//
// Reads carry grants too: some of them hide money totals from an operator
// without PermViewTotals.
func EnforceOperatorPermissions(tokens auth.TokenService, users userRepo.UserRepository, uow sharedDomain.UnitOfWork) gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			c.Next()
			return
		}
		claims, err := tokens.ValidateAccessToken(strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			// AuthMiddleware answers 401 on authenticated routes.
			c.Next()
			return
		}
		grants := resolveGrants(c, claims, users, uow)
		c.Request = c.Request.WithContext(sharedDomain.WithGrants(c.Request.Context(), grants))
		if p, gated := operatorRoutePermissions[c.Request.Method+" "+c.FullPath()]; gated && !grants.Allows(p) {
			AbortPermissionDenied(c, p)
			return
		}
		c.Next()
	}
}

func resolveGrants(c *gin.Context, claims auth.Claims, users userRepo.UserRepository, uow sharedDomain.UnitOfWork) sharedDomain.Grants {
	// Same trust as RequireOwner: the role in the signed token.
	if claims.Role == "owner" {
		return sharedDomain.OwnerGrants()
	}
	none := sharedDomain.OperatorGrants(nil)
	if users == nil || uow == nil {
		return none
	}
	tx, err := uow.Query(sharedDomain.WithTenant(c.Request.Context(), claims.GymID))
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		return none
	}
	u, err := users.GetByID(tx, claims.UserID)
	if err != nil || u == nil || u.GymID != claims.GymID || !u.Active {
		return none
	}
	return u.Grants()
}

// AbortPermissionDenied answers 403 with the Spanish message the desktop
// shows as is (it reads `exception`).
func AbortPermissionDenied(c *gin.Context, p sharedDomain.Permission) {
	msg := sharedDomain.PermissionDeniedMessage(p)
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"status_code": http.StatusForbidden,
		"message":     msg,
		"exception":   msg,
		"error":       "permission_denied",
		"permission":  string(p),
	})
}
