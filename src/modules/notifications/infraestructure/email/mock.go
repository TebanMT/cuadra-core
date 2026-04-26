package email

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// MockSent records one email captured by the mock provider.
type MockSent struct {
	To      string
	Subject string
	Body    string
	Tag     string
	At      time.Time
}

// MockProvider implements EmailProvider for tests + dev. Captures every
// send for assertion.
type MockProvider struct {
	mu   sync.Mutex
	Sent []MockSent
}

func NewMockProvider() *MockProvider { return &MockProvider{} }

func (m *MockProvider) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sent = nil
}

func (m *MockProvider) SendTransactional(_ context.Context, to, subject, body, tag string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sent = append(m.Sent, MockSent{
		To:      to,
		Subject: subject,
		Body:    body,
		Tag:     tag,
		At:      time.Now().UTC(),
	})
	return fmt.Sprintf("mock-%d", time.Now().UnixNano()), nil
}

// SentCount returns how many emails have been captured.
func (m *MockProvider) SentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Sent)
}

// StdoutProvider mirrors MockProvider but prints to log instead of capturing.
// Used as the dev / sidecar default. Sidecar doesn't actually send email —
// any flow that would email goes through cloud, which uses the real
// ResendProvider.
type StdoutProvider struct{}

func NewStdoutProvider() *StdoutProvider { return &StdoutProvider{} }

func (StdoutProvider) SendTransactional(_ context.Context, to, subject, body, tag string) (string, error) {
	log.Printf("[email/stdout] tag=%s to=%s subject=%q body=%q", tag, to, strings.TrimSpace(subject), strings.TrimSpace(body))
	return fmt.Sprintf("stdout-%d", time.Now().UnixNano()), nil
}
