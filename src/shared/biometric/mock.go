//go:build !sidecar

package biometric

import "context"

// MockReader is the default for cloud + tests. Capture returns a deterministic
// blob and Identify never matches. Sidecar build replaces it with the real
// DigitalPersona SDK adapter.
type MockReader struct{}

func NewMockReader() *MockReader { return &MockReader{} }

func (MockReader) Capture(_ context.Context) (Template, error) {
	return nil, ErrNotAvailable
}

func (MockReader) Identify(_ context.Context, _ Template, _ map[string]Template, _ float64) (MatchResult, error) {
	return MatchResult{}, ErrNoMatch
}

func (MockReader) Available(_ context.Context) bool { return false }
