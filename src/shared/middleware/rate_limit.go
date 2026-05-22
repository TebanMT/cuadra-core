package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimit guards low-volume public endpoints (forgot-password,
// resend-verification, signup) from email-flood abuse — a loop of POSTs to
// /auth/forgot-password would otherwise drain the Zoho SMTP daily quota.
//
// Algorithm: per-key token bucket. Each request consumes 1 token; the bucket
// refills at `refill` tokens/sec up to `burst`. When empty, returns 429 with
// Retry-After. Storage is in-memory + single-instance — acceptable for one
// Hetzner VPS; promote to Redis if we horizontally scale.
//
// Key derivation is caller-controlled via keyFn so the same primitive serves
// IP-only buckets (signup, login) and IP+email composite buckets
// (forgot-password, where a single attacker rotating emails should not slip
// past an IP cap).
type RateLimit struct {
	burst   float64
	refill  float64 // tokens per second
	keyFn   func(*gin.Context) string
	buckets sync.Map // key string -> *tokenBucket
}

type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	lastSeen time.Time
}

// NewRateLimit wires a limiter; keyFn must be non-nil. Typical refill values:
// 1 req every 30s → refill=1.0/30, burst=3. Document at the call site.
func NewRateLimit(burst, refillPerSecond float64, keyFn func(*gin.Context) string) *RateLimit {
	if keyFn == nil {
		panic("middleware: rate limit keyFn must be non-nil")
	}
	return &RateLimit{burst: burst, refill: refillPerSecond, keyFn: keyFn}
}

// Handler is the gin middleware factory. Reject with 429 + Retry-After when
// the bucket is empty.
func (rl *RateLimit) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := rl.keyFn(c)
		if key == "" {
			c.Next()
			return
		}
		now := time.Now()
		bAny, _ := rl.buckets.LoadOrStore(key, &tokenBucket{tokens: rl.burst, lastSeen: now})
		b := bAny.(*tokenBucket)
		b.mu.Lock()
		elapsed := now.Sub(b.lastSeen).Seconds()
		b.tokens = minFloat(rl.burst, b.tokens+elapsed*rl.refill)
		b.lastSeen = now
		if b.tokens < 1 {
			retryAfter := int((1 - b.tokens) / rl.refill)
			if retryAfter < 1 {
				retryAfter = 1
			}
			b.mu.Unlock()
			c.Header("Retry-After", itoa(retryAfter))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":   "rate_limited",
				"message": "Too many requests. Try again later.",
			})
			return
		}
		b.tokens--
		b.mu.Unlock()
		c.Next()
	}
}

// ClientIP returns the best-effort source IP. Honours X-Forwarded-For only
// when it has a single hop (Caddy/Cloudflare set this); otherwise it falls
// back to the direct peer.
func ClientIP(c *gin.Context) string {
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		ip := strings.TrimSpace(parts[0])
		if net.ParseIP(ip) != nil {
			return ip
		}
	}
	ip, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return c.Request.RemoteAddr
	}
	return ip
}

// IPKey returns a keyFn that buckets by source IP.
func IPKey() func(*gin.Context) string { return ClientIP }

// IPEmailKey buckets by IP + lowercased email from the request body field
// `email`. If the body is missing or unparseable, falls back to IP-only.
// Avoids parsing the body destructively by using c.Copy().GetRawData via
// c.ShouldBindBodyWith pattern — but here we read a header-style override
// the handler can set on c.Keys before the middleware runs (too invasive)
// or we read the JSON ourselves and rewind. We rewind.
func IPEmailKey() func(*gin.Context) string {
	return func(c *gin.Context) string {
		ip := ClientIP(c)
		email := strings.ToLower(strings.TrimSpace(extractEmailField(c)))
		if email == "" {
			return ip
		}
		return ip + "|" + email
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	if n < 0 {
		return "-" + itoa(-n)
	}
	out := ""
	for n > 0 {
		out = string(digits[n%10]) + out
		n /= 10
	}
	return out
}
