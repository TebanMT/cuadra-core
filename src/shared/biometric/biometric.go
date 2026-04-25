// Package biometric exposes a thin Reader interface around the DigitalPersona
// SDK (ADR-004). Sesión 1 doesn't use it (fingerprint enrollment is UC-028 in
// Sesión 5), but main.go wires the right impl by build tag so we don't pull
// SDK deps into cloud builds.
package biometric

import (
	"context"
	"errors"
)

// Template is an opaque biometric template (post-extraction, before the
// gym-master-key encryption layer in ADR-006).
type Template []byte

// MatchResult is what Identify returns. Score is implementation-defined; the
// caller (UC-029) compares against a threshold.
type MatchResult struct {
	MemberID string
	Score    float64
}

// Reader is the surface the use cases use. The sidecar wires the real SDK,
// the cloud build wires the mock so it can compile and run integration tests.
type Reader interface {
	// Capture blocks until the user places a finger or ctx fires. Returns the
	// extracted template ready for encryption.
	Capture(ctx context.Context) (Template, error)
	// Identify performs a 1:N match against the provided enrolled templates.
	// Returns the highest-scoring entry above the threshold, or
	// ErrNoMatch if nothing crosses it.
	Identify(ctx context.Context, input Template, enrolled map[string]Template, threshold float64) (MatchResult, error)
	// Available reports whether the underlying device is plugged in. UI hides
	// the fingerprint option when this is false.
	Available(ctx context.Context) bool
}

var (
	ErrNotAvailable = errors.New("biometric reader not available")
	ErrNoMatch      = errors.New("no match")
)
