//go:build server

package main

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"strings"
)

// Keep existing Tinta diagnostics. Personal auth has a separate boundary:
// default Gin panic reports dump headers, including attempt/result secrets.
func newServerRouter() *gin.Engine {
	r := gin.New()
	ordinaryLog := gin.Logger()
	ordinaryRecovery := gin.Recovery()
	personal := func(c *gin.Context) bool { return strings.HasPrefix(c.Request.URL.Path, "/api/mancuerna/") }
	r.Use(func(c *gin.Context) {
		if personal(c) {
			c.Next()
		} else {
			ordinaryLog(c)
		}
	})
	r.Use(func(c *gin.Context) {
		if !personal(c) {
			ordinaryRecovery(c)
			return
		}
		defer func() {
			if recover() != nil {
				id := uuid.NewString()
				c.Header("X-Request-ID", id)
				c.Header("Cache-Control", "no-store")
				c.Header("Pragma", "no-cache")
				c.AbortWithStatusJSON(503, gin.H{"code": "TEMPORARY_UNAVAILABLE", "message": "No pudimos confirmar el resultado. Consulta el estado antes de reintentar.", "retryable": true, "request_id": id, "action": "retry", "fields": []any{}})
			}
		}()
		c.Next()
	})
	return r
}
