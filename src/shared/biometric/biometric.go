// Package biometric exposes a thin Reader interface around the matching
// engine (ADR-004 + ADR-004-bis). The cloud build wires the Mock
// implementation; the sidecar build wires the NBIS adapter
// (mindtct + bozorth3 subprocesses, ANSI INCITS 378 templates).
//
// Capture (talking to the U.are.U 4500) lives in the Tauri frontend via
// @digitalpersona/fingerprint per ADR-004-bis §2 — the sidecar receives a
// raw PNG over HTTP and turns it into a template via ExtractTemplate. The
// Capture / Enroll methods on Reader remain on the interface for older
// callers but return ErrCaptureMovedToFrontend on the NBIS implementation.
//
// Everything heavier — template encryption, GMK lookup, persistence — lives
// one layer up so this package can be swapped for any matcher that consumes
// ANSI 378 templates (ADR-004 §4.3, ADR-004-bis §6).
package biometric

import (
	"context"
	"errors"
)

// Template is an opaque biometric template (post-extraction, before the
// gym-master-key encryption layer in ADR-006).
type Template []byte

// CaptureResult is what ExtractTemplate hands back after turning a raw
// fingerprint image into a feature-extracted template. Bytes are the
// *plaintext* template — callers MUST encrypt before persisting (see
// shared/biometric/crypto). QualityScore is the matcher's own 0-100
// self-assessment based on minutiae count and distribution; UC-028 rejects
// samples below QualityScoreFloor.
//
// In the NBIS pipeline (ADR-004-bis) Bytes is the UTF-8 contents of an .xyt
// file as produced by `mindtct -m1` — one minutia per line, "x y theta
// quality", coordinates following ANSI INCITS 378-2004 conventions. Bozorth3
// reads the same shape with `-m1`, so probe and gallery never need conversion.
type CaptureResult struct {
	Bytes        Template
	Format       string // "xyt_m1" in the NBIS pipeline (ANSI INCITS 378 coords)
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

	// Capture is a no-op on the NBIS implementation — capture lives in the
	// Tauri frontend (ADR-004-bis §2). Kept on the interface so older code
	// paths (cloud build, tests) compile unchanged; the NBIS matcher returns
	// ErrCaptureMovedToFrontend. Mock + cloud implementations still honor it
	// for unit tests that don't care about hardware origin.
	Capture(ctx context.Context) (*CaptureResult, error)

	// Enroll, like Capture, is now driven by the frontend (multi-sample
	// loop in JS, ADR-004-bis §2). Same fallback contract as Capture.
	Enroll(ctx context.Context, samples int) (*CaptureResult, error)

	// ExtractTemplate turns a raw fingerprint image (PNG bytes from the
	// frontend's PngImage capture, ~170 KB) into a feature-extracted template
	// ready for Identify / persistence. The NBIS implementation invokes
	// `mindtct -m1` as a subprocess; the mock returns a deterministic stub.
	//
	// Returns ErrQualityThreshold if the matcher can't extract enough
	// minutiae for a usable template (typical threshold: ≥20 minutiae).
	ExtractTemplate(ctx context.Context, image []byte) (*CaptureResult, error)

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
	ErrNotAvailable            = errors.New("biometric reader not available")
	ErrNoMatch                 = errors.New("no fingerprint match above threshold")
	ErrQualityThreshold        = errors.New("fingerprint quality below threshold")
	ErrNoFingerDetected        = errors.New("no finger detected on reader")
	ErrCaptureMovedToFrontend  = errors.New("capture lives in frontend per ADR-004-bis; call ExtractTemplate with image bytes")
	ErrTemplateExtractFailed   = errors.New("mindtct could not extract a usable template from the image")
)
