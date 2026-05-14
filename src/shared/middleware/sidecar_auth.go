// SidecarOrJWTMiddleware is referenced by auth_controller.go (no build
// tag) at the route registration site, so the symbol must exist in both
// builds. The implementation only uses interfaces (auth.TokenService,
// sidecartoken.Store) that are available without a build tag; the
// sidecar binary just never wires this middleware (it has its own
// SidecarAuthProxy).

package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/sidecartoken"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// AuthMethodKey lets sync metrics tag a request as authenticated by
// `sidecar_token` vs `jwt`. The sync handler reads this from context and
// increments the corresponding counter (ADR-008 §7).
const AuthMethodKey = "cuadra_auth_method"

// SidecarOrJWTMiddleware is the hybrid auth gate for /sync/* (ADR-008
// §3.2). Tokens with the `sk_live_` prefix go through the sidecar table;
// everything else falls through to the operator JWT path. The fallback
// remains in place during the migration window so a sidecar that hasn't
// upgraded yet can keep syncing on its JWT.
//
// last_seen_at is touched on every successful sidecar-token request via a
// goroutine so the request itself doesn't block on the UPDATE.
func SidecarOrJWTMiddleware(tokens auth.TokenService, store sidecartoken.Store) gin.HandlerFunc {
	jwtPath := AuthMiddleware(tokens)
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" || !strings.HasPrefix(header, "Bearer ") {
			utils.ErrorResponse(c, http.StatusUnauthorized, errUnauthorized)
			c.Abort()
			return
		}
		bearer := strings.TrimPrefix(header, "Bearer ")
		if !sidecartoken.HasPrefix(bearer) {
			c.Set(AuthMethodKey, "jwt")
			jwtPath(c)
			return
		}

		hash := sidecartoken.Hash(bearer)
		cred, err := store.LookupActiveByHash(c.Request.Context(), hash)
		if err != nil {
			if errors.Is(err, sidecartoken.ErrNotFound) {
				utils.ErrorResponse(c, http.StatusUnauthorized, errUnauthorized)
				c.Abort()
				return
			}
			utils.ErrorResponse(c, http.StatusInternalServerError, err)
			c.Abort()
			return
		}
		c.Set(GymIDKey, cred.GymID)
		// User context comes from the credential's emitter for audit
		// breadcrumbs — sync writes don't have a "current user" since the
		// sidecar runs 24/7 detached from any session, but the bootstrapper
		// is still the closest thing.
		c.Set(UserIDKey, cred.UserID)
		c.Set(AuthMethodKey, "sidecar_token")

		// Fire-and-forget last_seen_at update. We intentionally use a
		// detached background context so the UPDATE survives the request
		// returning. If it fails we accept staleness — the touch is for
		// idle-revoke, not security.
		credID := cred.ID
		go func() {
			ctx, cancel := detachedContext()
			defer cancel()
			_ = store.TouchLastSeen(ctx, credID)
		}()
		c.Next()
	}
}

// GetAuthMethod returns "sidecar_token", "jwt", or empty string when no
// authentication ran.
func GetAuthMethod(c *gin.Context) string {
	v, ok := c.Get(AuthMethodKey)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
