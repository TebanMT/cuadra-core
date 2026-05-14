// detachedContext is referenced by sidecar_auth.go which is built for
// both server and sidecar (cf. comment in sidecar_auth.go). Sin tag
// para que el binario del sidecar compile aunque no use el middleware.

package middleware

import (
	"context"
	"time"
)

// detachedContext returns a fresh background-derived context with a
// generous timeout and the matching cancel func. Callers must defer the
// cancel. Used for fire-and-forget DB writes that outlive the HTTP request
// (e.g. last_seen_at update on /sync/*).
func detachedContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
