package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// CORSConfig configures the CORS middleware. AllowedOrigins is an exact-match
// list of origins (e.g. "http://localhost:5173"). The single value "*" makes
// the middleware echo any incoming Origin header — convenient for local dev
// but never appropriate for a public deployment.
type CORSConfig struct {
	AllowedOrigins []string
	AllowedHeaders []string
}

// CORS returns a Gin middleware that handles CORS preflight and adds the
// response headers required for cross-origin browser requests. It must be
// installed before any auth middleware so OPTIONS preflights — which carry
// no auth headers — are answered before the auth gate aborts them.
func CORS(cfg CORSConfig) gin.HandlerFunc {
	allowAny := len(cfg.AllowedOrigins) == 1 && cfg.AllowedOrigins[0] == "*"
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		allowed[o] = struct{}{}
	}

	headers := "Authorization, Content-Type, X-Local-Token, X-Requested-With"
	if len(cfg.AllowedHeaders) > 0 {
		headers = strings.Join(cfg.AllowedHeaders, ", ")
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		c.Header("Vary", "Origin")

		if origin != "" {
			_, ok := allowed[origin]
			if allowAny || ok {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Access-Control-Allow-Credentials", "true")
				c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				c.Header("Access-Control-Allow-Headers", headers)
				c.Header("Access-Control-Max-Age", "600")
			}
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
