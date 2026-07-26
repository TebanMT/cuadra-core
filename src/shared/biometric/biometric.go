// Package biometric wraps the tinta-bio helper — the native biometric engine
// (C#/.NET 8, DigitalPersona SDK + FingerJet) that owns capture, extraction,
// enrollment and 1:N identification (ADR-004, pivote jul-2026 a Ruta A).
//
// The sidecar spawns tinta-bio.exe as a child process and speaks NDJSON over
// stdin/stdout (contract: tools/tinta-bio/PROTOCOL.md). Engine in this package
// is that process manager: spawn + supervise + restart with backoff, command
// round-trips (ping / gallery / identify / enroll) and spontaneous events
// (reader hot-plug, samples) fanned out to a Handler.
//
// FMDs (FingerJet templates, ANSI 378 inside) are OPAQUE strings — base64 of
// the SDK's XML serialization. This package never parses them; the layer
// above encrypts them with the GMK (shared/biometric/crypto), persists them
// and hands them back verbatim as gallery candidates.
//
// Orchestration (gallery ownership, enroll sessions, check-in flow) lives in
// the checkins application layer (BiometricHub) — this package stays a dumb,
// swappable pipe to the engine process.
package biometric

import "errors"

// Template is an opaque biometric template as stored (pre-GMK-encryption).
// In the tinta-bio pipeline it is the raw bytes of the FMD string.
type Template []byte

// FormatFMD is the template_format stamped on member_fingerprints rows
// produced by the tinta-bio helper: base64 of the DigitalPersona SDK's XML
// FMD serialization (ANSI 378 minutiae inside). Legacy formats (dp_uareu,
// xyt_m1) never reached production — no migration path is needed.
const FormatFMD = "fmd_xml_b64"

// DefaultFarDivisor mirrors the helper's default: target FAR = 1/divisor on
// identify. Same value as the official SDK sample.
const DefaultFarDivisor = 100_000

// CaptureResult is one extracted template handed to the members use cases
// (RegisterFingerprint). Bytes is the *plaintext* template — callers MUST
// encrypt before persisting (see shared/biometric/crypto). QualityScore is
// optional (0 = not reported); the helper reports categorical quality
// (DP_QUALITY_GOOD) rather than a 0-100 score, so FMD captures leave it 0.
type CaptureResult struct {
	Bytes        Template
	Format       string
	QualityScore int
}

// GalleryCandidate is one entry of the helper's cached 1:N gallery. Ref is
// the member_fingerprints.id — the sidecar resolves ref→member after a
// match. FMD is the decrypted opaque template string.
type GalleryCandidate struct {
	Ref string `json:"ref"`
	FMD string `json:"fmd"`
}

// ReaderInfo is what `GET /api/v1/biometric/status` exposes. Model/DeviceID
// come from the helper's reader events (name + serial).
type ReaderInfo struct {
	DeviceID  string
	Vendor    string
	Model     string
	Connected bool
}

// Handler receives the engine's spontaneous notifications. Implemented by
// the BiometricHub (checkins app layer). Calls are delivered one at a time
// from a single dispatch goroutine, in arrival order — implementations may
// block briefly (identify + DB work) and rely on that serialization.
type Handler interface {
	// HandleSample fires per successful capture+extraction (a "dedazo").
	HandleSample(fmd, quality string)
	// HandleSampleRejected fires when a capture happened but quality or
	// extraction failed — feedback for "vuelve a apoyar el dedo".
	HandleSampleRejected(code, quality string)
	// HandleReaderState reflects reader hot-plug. name/serial are only
	// meaningful when connected.
	HandleReaderState(connected bool, name, serial string)
	// HandleHelperUp fires after the helper process (re)spawns. The hub
	// re-sends the gallery here — a fresh helper starts with an empty one.
	HandleHelperUp()
	// HandleHelperDown fires when the helper process dies (it will be
	// restarted with backoff unless the engine is stopping).
	HandleHelperDown(reason string)
}

var (
	// ErrNotAvailable — the engine process isn't running/responsive (helper
	// missing, restarting, or stopped).
	ErrNotAvailable = errors.New("motor biométrico no disponible")

	// ErrHelperRestarted — the in-flight command died with the helper
	// process; the caller may retry after the next HandleHelperUp.
	ErrHelperRestarted = errors.New("el motor biométrico se reinició a media operación")
)

// CommandError is a helper-reported command failure ({"ok":false,...}), e.g.
// DP_ENROLLMENT_INVALID_SET when the enroll FMD set doesn't converge.
type CommandError struct {
	Code string
}

func (e *CommandError) Error() string {
	if e.Code == "" {
		return "el motor biométrico rechazó el comando"
	}
	return "el motor biométrico rechazó el comando: " + e.Code
}
