package middleware

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/gin-gonic/gin"
)

// extractEmailField peeks at a JSON body looking for an `email` field and
// rewinds c.Request.Body so the actual handler can re-read it. Returns "" if
// the body is not JSON, the field is missing, or the body is large enough
// that scanning would be wasteful (max 8 KiB — the email-bearing endpoints
// have tiny payloads in practice).
func extractEmailField(c *gin.Context) string {
	if c.Request == nil || c.Request.Body == nil {
		return ""
	}
	const maxPeek = 8 * 1024
	buf := &bytes.Buffer{}
	// Read up to maxPeek bytes; rewind the body so the handler is oblivious
	// to the peek.
	limited := io.LimitReader(c.Request.Body, maxPeek+1)
	if _, err := io.Copy(buf, limited); err != nil {
		_ = c.Request.Body.Close()
		c.Request.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
		return ""
	}
	body := buf.Bytes()
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > maxPeek {
		return ""
	}
	var probe struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Email
}
