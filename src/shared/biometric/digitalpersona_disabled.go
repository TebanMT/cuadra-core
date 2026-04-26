//go:build sidecar && !bio_dp

package biometric

import (
	"context"
	"log"
	"sync"
)

// DigitalPersonaReader (default sidecar build) is the minimal stub the
// development sidecar uses when neither `bio_dp` (real SDK linkage) nor
// `bio_mock` (test mock) is set. It satisfies the Reader contract by
// returning ErrNotAvailable everywhere, mirroring "no device plugged in" so
// the kiosk loop falls back to PIN/manual without crashing.
//
// Build tag matrix:
//
//	go build -tags=sidecar               → this file (dev sidecar, no hardware)
//	go build -tags="sidecar bio_dp"      → digitalpersona.go (prod sidecar)
//	go test  -tags="sidecar bio_mock"    → mock.go (sidecar tests)
//	go build                              → mock.go (cloud)
type DigitalPersonaReader struct {
	mu            sync.Mutex
	connectCBs    []func()
	disconnectCBs []func()
}

func NewDigitalPersonaReader() *DigitalPersonaReader {
	log.Printf("[biometric] DigitalPersona reader DISABLED (build sin tag bio_dp ni bio_mock) — kiosko opera con PIN/manual únicamente")
	return &DigitalPersonaReader{}
}

func (r *DigitalPersonaReader) Info() ReaderInfo {
	return ReaderInfo{
		DeviceID:  "",
		Vendor:    "HID/Crossmatch",
		Model:     "U.are.U 4500 (disabled at build)",
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
	return nil, ErrNotAvailable
}

func (*DigitalPersonaReader) Enroll(_ context.Context, _ int) (*CaptureResult, error) {
	return nil, ErrNotAvailable
}

func (*DigitalPersonaReader) Identify(_ context.Context, _ *CaptureResult, _ []EncryptedTemplate, _ float64) (*MatchResult, error) {
	return nil, ErrNoMatch
}

func (*DigitalPersonaReader) Available(_ context.Context) bool { return false }
