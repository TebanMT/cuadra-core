//go:build sidecar && bio_dp

package biometric

import (
	"context"
	"log"
	"sync"

	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
)

// DigitalPersonaReader will wrap the DigitalPersona U.are.U SDK (ADR-004 §3,
// §4.4). The build tag `bio_dp` keeps this file out of CI / dev builds where
// the native lib isn't installed; only release builds for the kiosko target
// it.
//
// The CGO declarations below are commented out to keep the file compilable
// without the SDK present. When the real lib lands:
//
//   - drop the leading `// ` from the `// #cgo …` block,
//   - drop the leading `// ` from the `// #include "dpfpdd.h"` line,
//   - replace the stubbed methods with real SDK calls.
//
// TODO: ADR-004 Phase 1 — link real DP SDK.
//
// /*
// #cgo CFLAGS: -I/Library/Frameworks/Crossmatch.framework/Headers
// #cgo LDFLAGS: -framework Crossmatch -framework CoreFoundation
// #include <stdlib.h>
// #include "dpfpdd.h"
// */
// import "C"
type DigitalPersonaReader struct {
	mu sync.Mutex

	GMK   bcrypto.GMKProvider
	GymID interface{} // uuid.UUID; kept untyped here to avoid uuid dep in stub

	connectCBs    []func()
	disconnectCBs []func()
}

func NewDigitalPersonaReader() *DigitalPersonaReader {
	log.Printf("[biometric] DigitalPersona reader stubbed — ADR-004 Phase 1 SDK linkage pendiente")
	return &DigitalPersonaReader{}
}

func (r *DigitalPersonaReader) Info() ReaderInfo {
	return ReaderInfo{
		DeviceID:  "",
		Vendor:    "HID/Crossmatch",
		Model:     "U.are.U 4500 (stub)",
		Connected: false,
	}
}

func (r *DigitalPersonaReader) OnConnect(cb func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectCBs = append(r.connectCBs, cb)
}

func (r *DigitalPersonaReader) OnDisconnect(cb func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disconnectCBs = append(r.disconnectCBs, cb)
}

func (*DigitalPersonaReader) Capture(_ context.Context) (*CaptureResult, error) {
	// TODO: ADR-004 Phase 1 — call dpfpdd_capture, run dpfpdd_create_ftrs to
	// extract the template, return CaptureResult with the SDK quality score.
	return nil, ErrNotAvailable
}

func (*DigitalPersonaReader) Enroll(_ context.Context, _ int) (*CaptureResult, error) {
	// TODO: ADR-004 Phase 1 — loop dpfpdd_capture × samples, dpfpdd_join_ftrs
	// to combine, then dpfpdd_create_enrollment for the final blob.
	return nil, ErrNotAvailable
}

func (*DigitalPersonaReader) Identify(_ context.Context, _ *CaptureResult, _ []EncryptedTemplate, _ float64) (*MatchResult, error) {
	// TODO: ADR-004 Phase 1 — decrypt each EncryptedTemplate via GMK, then
	// dpfpdd_identify against the in-memory candidate set with threshold.
	return nil, ErrNoMatch
}

func (*DigitalPersonaReader) Available(_ context.Context) bool { return false }
