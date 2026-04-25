//go:build sidecar

package biometric

import (
	"context"
	"log"
)

// DigitalPersonaReader will wrap the DigitalPersona U.are.U SDK (ADR-004).
// The real bindings live in a future PR; for now the sidecar build also
// returns "not available" so we can compile and ship Sesión 1 without the
// hardware on the dev machine. UC-028 (Sesión 5) replaces this body.
//
// TODO(humano): integrar SDK real (DigitalPersona U.are.U 4500 driver) — ver
// ADR-004 §3 para flow + ADR-006 §2.4 para encriptación con la GMK del gym.
type DigitalPersonaReader struct{}

func NewDigitalPersonaReader() *DigitalPersonaReader {
	log.Printf("[biometric] DigitalPersona reader stubbed — ADR-004 SDK pendiente")
	return &DigitalPersonaReader{}
}

func (DigitalPersonaReader) Capture(_ context.Context) (Template, error) {
	return nil, ErrNotAvailable
}

func (DigitalPersonaReader) Identify(_ context.Context, _ Template, _ map[string]Template, _ float64) (MatchResult, error) {
	return MatchResult{}, ErrNoMatch
}

func (DigitalPersonaReader) Available(_ context.Context) bool { return false }
