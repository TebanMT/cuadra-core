// Package biometric exposes a thin Reader interface around the DigitalPersona
// SDK (ADR-004). The cloud build wires the Mock implementation; the sidecar
// build wires the real DP adapter (currently a stub — TODO Phase 1 of ADR-004).
//
// The interface deliberately stays small: capture / enroll / identify / info,
// plus async connect/disconnect callbacks the kiosk loop subscribes to
// (UC-031). Everything heavier — template encryption, GMK lookup, persistence
// — lives one layer up so this package can be swapped for any vendor that
// exposes equivalent primitives (ADR-004 §4.3).
package biometric

import (
	"context"
	"errors"
)

// Template is an opaque biometric template (post-extraction, before the
// gym-master-key encryption layer in ADR-006).
type Template []byte

// CaptureResult is what the SDK hands back after a successful capture or
// enroll. Bytes are the *plaintext* template — callers MUST encrypt before
// persisting (see shared/biometric/crypto). QualityScore is the SDK's own
// 0-100 self-assessment; UC-028 rejects samples below 60.
type CaptureResult struct {
	Bytes        Template
	Format       string // "dp_uareu" in the MVP — leaves room for ANSI-378 etc.
	QualityScore int
}

// EncryptedTemplate is one row from member_fingerprints handed to Identify.
// Decryption happens inside the Reader implementation so the plaintext never
// crosses the use-case boundary (DA-29 §"100% offline + en memoria").
type EncryptedTemplate struct {
	MemberID string
	Bytes    []byte // [version][IV][ciphertext][tag] per ADR-006 §2.1
	Format   string
}

// MatchResult is what Identify returns. Score is implementation-defined; the
// caller (UC-029) compares against a threshold from gyms.kiosk_settings.
type MatchResult struct {
	MemberID string
	Score    float64
}

// ReaderInfo is what `GET /api/v1/biometric/status` exposes. The sidecar
// queries it on boot + on every connect callback so the UI can hide the
// fingerprint option when no device is plugged in (UC-032 DA-32.4).
type ReaderInfo struct {
	DeviceID  string
	Vendor    string // e.g. "HID/Crossmatch"
	Model     string // e.g. "U.are.U 4500"
	Connected bool
}

// Reader is the surface the use cases use. Sidecar wires the real SDK; cloud
// + tests wire the mock.
type Reader interface {
	// Info returns vendor/model/connected. Cheap; safe to call frequently.
	Info() ReaderInfo

	// OnConnect / OnDisconnect register hot-plug callbacks. The kiosk loop uses
	// them to surface banners ("Lector conectado / desconectado") to the UI
	// without polling. Implementations should fire callbacks on a goroutine so
	// the SDK thread is never blocked.
	OnConnect(cb func())
	OnDisconnect(cb func())

	// Capture blocks until the user places a finger or ctx fires. Returns the
	// extracted plaintext template ready for downstream encryption.
	Capture(ctx context.Context) (*CaptureResult, error)

	// Enroll combines `samples` captures (DP recommends 3) into a robust
	// template. The implementation handles per-sample retry on quality failure;
	// it returns only when all samples have been merged or ctx fires.
	Enroll(ctx context.Context, samples int) (*CaptureResult, error)

	// Identify performs a 1:N match against the provided enrolled blobs. The
	// Reader is responsible for decrypting `enrolled` (using the GMKProvider
	// passed at construction). Returns the highest-scoring entry above the
	// threshold, or ErrNoMatch if nothing crosses it.
	Identify(ctx context.Context, input *CaptureResult, enrolled []EncryptedTemplate, threshold float64) (*MatchResult, error)

	// Available is a fast yes/no — the kiosk loop polls it on start and any
	// time it suspects the SDK lost the device between callbacks.
	Available(ctx context.Context) bool
}

var (
	ErrNotAvailable     = errors.New("biometric reader not available")
	ErrNoMatch          = errors.New("no fingerprint match above threshold")
	ErrQualityThreshold = errors.New("fingerprint quality below threshold")
	ErrNoFingerDetected = errors.New("no finger detected on reader")
)
