//go:build !sidecar || bio_mock

package biometric

import (
	"context"
	"sync"

	"github.com/google/uuid"

	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
)

// MockReader is a fully wired in-memory implementation used by:
//   - cloud build (`!sidecar`) — so server compiles + can run integration tests.
//   - sidecar tests (`sidecar && bio_mock`) — drives UC-029 end-to-end without
//     touching real hardware.
//
// The "match" decision is fake-but-deterministic: an enrolled blob matches an
// input CaptureResult whose Bytes are exactly equal to its decrypted contents
// (the test helper EnrollFor calls Encrypt internally, then Identify will
// decrypt + compare). This keeps the mock free of any "matcher" mock state
// that diverges from the real DP semantics.
type MockReader struct {
	mu sync.Mutex

	// GMK provider used to decrypt enrolled blobs in Identify(). Tests inject
	// the same provider used by the use case so encrypt/decrypt is symmetric.
	GMK bcrypto.GMKProvider
	// GymID lets Identify look up the right GMK without each call having to
	// pass it. Tests Set this once at fixture setup.
	GymID uuid.UUID

	// available toggles the simulated hot-plug state. Default true so a freshly
	// constructed MockReader behaves "as if" the device is present.
	available    bool
	connectCBs   []func()
	disconnectCB []func()

	// nextCapture is what the next Capture()/Enroll() call returns. Tests stage
	// it via QueueCapture; if empty, the readers return ErrNoFingerDetected.
	nextCapture *CaptureResult

	// info is reported by Info(). Defaults to a sensible mock; tests can
	// override with SetInfo to simulate other vendors.
	info ReaderInfo
}

// NewMockReader returns a MockReader with a default "connected DP4500" identity
// and `available=true`. Tests usually do:
//
//	r := biometric.NewMockReader()
//	r.GMK = gmkProvider
//	r.GymID = gymID
func NewMockReader() *MockReader {
	return &MockReader{
		available: true,
		info: ReaderInfo{
			DeviceID:  "mock-0001",
			Vendor:    "Cuadra/Mock",
			Model:     "MockReader v0",
			Connected: true,
		},
	}
}

func (m *MockReader) Info() ReaderInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	info := m.info
	info.Connected = m.available
	return info
}

func (m *MockReader) SetInfo(info ReaderInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.info = info
}

func (m *MockReader) OnConnect(cb func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connectCBs = append(m.connectCBs, cb)
}

func (m *MockReader) OnDisconnect(cb func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disconnectCB = append(m.disconnectCB, cb)
}

// SimulateConnect / SimulateDisconnect drive the hot-plug callbacks during
// kiosk tests. They run callbacks synchronously so tests can assert state
// without races.
func (m *MockReader) SimulateConnect() {
	m.mu.Lock()
	m.available = true
	cbs := append([]func(){}, m.connectCBs...)
	m.mu.Unlock()
	for _, cb := range cbs {
		cb()
	}
}

func (m *MockReader) SimulateDisconnect() {
	m.mu.Lock()
	m.available = false
	cbs := append([]func(){}, m.disconnectCB...)
	m.mu.Unlock()
	for _, cb := range cbs {
		cb()
	}
}

// QueueCapture stages the next CaptureResult that Capture() / Enroll() will
// return. The reader consumes it in a single call, so tests can sequence
// multiple captures by calling QueueCapture between them.
func (m *MockReader) QueueCapture(result *CaptureResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextCapture = result
}

func (m *MockReader) Capture(ctx context.Context) (*CaptureResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.available {
		return nil, ErrNotAvailable
	}
	if m.nextCapture == nil {
		return nil, ErrNoFingerDetected
	}
	out := m.nextCapture
	m.nextCapture = nil
	return out, nil
}

// Enroll in the mock is a thin wrapper over Capture — the SDK normally
// combines `samples` reads here, but for tests one staged capture is enough.
// The samples count is validated to keep the contract honest.
func (m *MockReader) Enroll(ctx context.Context, samples int) (*CaptureResult, error) {
	if samples < 1 {
		return nil, ErrQualityThreshold
	}
	return m.Capture(ctx)
}

// ExtractTemplate in the mock just returns the staged capture — tests that
// exercise the frontend → sidecar flow set `nextCapture` to the template
// they want the matcher to produce when it sees a particular "image". The
// image bytes themselves are ignored, since the mock has no real extractor.
func (m *MockReader) ExtractTemplate(ctx context.Context, _ []byte) (*CaptureResult, error) {
	return m.Capture(ctx)
}

// Identify decrypts each enrolled blob with the configured GMKProvider and
// returns the first one whose plaintext bytes equal input.Bytes. Score is
// always 1.0 on a hit (we don't simulate fuzzy matching in the mock).
func (m *MockReader) Identify(ctx context.Context, input *CaptureResult, enrolled []EncryptedTemplate, threshold float64) (*MatchResult, error) {
	if input == nil || len(input.Bytes) == 0 {
		return nil, ErrNoFingerDetected
	}
	if m.GMK == nil {
		return nil, ErrNotAvailable
	}
	gmk, err := m.GMK.GetGMK(ctx, m.GymID)
	if err != nil {
		return nil, err
	}
	defer bcrypto.Zero(gmk)
	for _, e := range enrolled {
		plain, err := bcrypto.DecryptTemplate(gmk, e.Bytes)
		if err != nil {
			continue
		}
		match := bytesEqual(plain, input.Bytes)
		bcrypto.Zero(plain)
		if match {
			return &MatchResult{MemberID: e.MemberID, Score: 1.0}, nil
		}
	}
	return nil, ErrNoMatch
}

func (m *MockReader) Available(_ context.Context) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.available
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
