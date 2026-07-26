package biometric

import (
	"context"
	"sync"
)

// MockEngine is the in-memory stand-in for the tinta-bio helper, used by the
// sidecar integration tests (build tag bio_mock) and any dev flow without
// hardware. It implements the same command surface as *Engine.
//
// Matching is fake-but-deterministic: in mock-land a finger IS its FMD
// string — every sample of that finger is the same string, EnrollCombine of
// N samples returns the first one, and Identify matches by exact string
// equality against the cached gallery. That keeps the mock free of matcher
// state that could diverge from the real engine's semantics.
//
// Events are NOT simulated here: tests drive the orchestration layer by
// calling its Handler methods (HandleSample etc.) directly, which mirrors
// how the real Engine delivers them (serialized, one at a time).
type MockEngine struct {
	mu sync.Mutex

	// Gallery/GalleryEpoch mirror what the last SetGallery call delivered.
	// Tests may mutate GalleryEpoch directly to simulate the enroll-vs-
	// identify race (helper holding a different gallery than the hub).
	Gallery      []GalleryCandidate
	GalleryEpoch string
	// SetGalleryCalls counts SetGallery invocations — the epoch-race tests
	// assert the hub re-sent the gallery.
	SetGalleryCalls int

	// EnrollErr, when set, is returned by EnrollCombine (simulates
	// DP_ENROLLMENT_INVALID_SET and friends).
	EnrollErr error
	// IdentifyErr, when set, is returned by Identify.
	IdentifyErr error

	alive     bool
	connected bool
	info      ReaderInfo
}

// NewMockEngine returns a MockEngine that reports a connected mock reader.
func NewMockEngine() *MockEngine {
	return &MockEngine{
		alive:     true,
		connected: true,
		info: ReaderInfo{
			DeviceID:  "mock-0001",
			Vendor:    "Tinta/Mock",
			Model:     "MockEngine v1",
			Connected: true,
		},
	}
}

func (m *MockEngine) SetGallery(_ context.Context, epoch string, candidates []GalleryCandidate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.alive {
		return ErrNotAvailable
	}
	m.Gallery = append([]GalleryCandidate{}, candidates...)
	m.GalleryEpoch = epoch
	m.SetGalleryCalls++
	return nil
}

func (m *MockEngine) Identify(_ context.Context, probeFMD string, _, max int) ([]string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.alive {
		return nil, "", ErrNotAvailable
	}
	if m.IdentifyErr != nil {
		return nil, m.GalleryEpoch, m.IdentifyErr
	}
	if max <= 0 {
		max = 1
	}
	matches := []string{}
	for _, c := range m.Gallery {
		if c.FMD == probeFMD {
			matches = append(matches, c.Ref)
			if len(matches) >= max {
				break
			}
		}
	}
	return matches, m.GalleryEpoch, nil
}

func (m *MockEngine) EnrollCombine(_ context.Context, fmds []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.alive {
		return "", ErrNotAvailable
	}
	if m.EnrollErr != nil {
		return "", m.EnrollErr
	}
	if len(fmds) == 0 {
		return "", &CommandError{Code: "empty_enrollment_fmd"}
	}
	return fmds[0], nil
}

func (m *MockEngine) Alive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.alive
}

func (m *MockEngine) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.alive && m.connected
}

func (m *MockEngine) Info() ReaderInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	info := m.info
	info.Connected = m.alive && m.connected
	return info
}

// SetAlive / SetConnected toggle the simulated process + hardware state.
func (m *MockEngine) SetAlive(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alive = v
}

func (m *MockEngine) SetConnected(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = v
}
