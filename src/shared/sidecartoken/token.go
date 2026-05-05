// Package sidecartoken implements the long-lived opaque credentials that
// authenticate sidecar↔cloud sync traffic (ADR-008 §3.2). The token format
// is `sk_live_<base64url(32 random bytes)>` — opaque, not enumerable, and
// stored server-side only as a SHA-256 hash. It is intentionally NOT a JWT
// because this credential never expires by clock; it is revoked by row.
package sidecartoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// Prefix is the leading magic string the middleware uses to dispatch
// between sidecar tokens and operator JWTs on the same Authorization
// header. Anything starting with this is treated as a sidecar token; the
// rest fall through to JWT validation.
const Prefix = "sk_live_"

// HasPrefix reports whether a bearer string is shaped like a sidecar token.
// The middleware uses this to choose which validator to run before doing
// any expensive work.
func HasPrefix(s string) bool {
	return strings.HasPrefix(s, Prefix)
}

// Generate mints a fresh opaque token and its SHA-256 hash. The plaintext
// is returned exactly once — the cloud persists only the hash.
func Generate() (token string, hash []byte, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, fmt.Errorf("sidecartoken: rand: %w", err)
	}
	token = Prefix + base64.RawURLEncoding.EncodeToString(raw[:])
	h := sha256.Sum256([]byte(token))
	return token, h[:], nil
}

// Hash returns the canonical SHA-256 of a token. Callers handle case
// matching (none — the token is base64url which is case-sensitive).
func Hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}
