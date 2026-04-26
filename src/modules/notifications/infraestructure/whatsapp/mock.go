package whatsapp

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain"
	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
)

// MockSent records one outbound message captured by the mock provider.
type MockSent struct {
	GymID          uuid.UUID
	Recipient      string
	TemplateKey    string
	Vars           map[string]string
	Body           string // rendered body (freeform) or empty for template sends
	IsFreeform     bool
	ProviderMsgID  string
	At             time.Time
	ForceFailRetry bool
}

// MockProvider implements WhatsAppProvider for tests + sidecar / dev. It
// captures every send for assertion and supports forcing failures so the
// dispatcher's retry path is testable.
type MockProvider struct {
	mu           sync.Mutex
	Sent         []MockSent
	FailNext     int  // when > 0, the next N calls return ErrSendFailedRetry
	FailNextHard bool // when true, the next call returns ErrSendFailedFinal
}

func NewMockProvider() *MockProvider {
	return &MockProvider{}
}

// Reset wipes captured state. Tests should call this between assertions
// when reusing the same instance across sub-tests.
func (m *MockProvider) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sent = nil
	m.FailNext = 0
	m.FailNextHard = false
}

func (m *MockProvider) SendTemplate(_ context.Context, cfg notiDomain.GymWhatsAppConfig, recipient, templateKey string, vars map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.FailNextHard {
		m.FailNextHard = false
		return "", notiErrors.ErrSendFailedFinal
	}
	if m.FailNext > 0 {
		m.FailNext--
		return "", notiErrors.ErrSendFailedRetry
	}

	def := tplDomain.LookupDefault(templateKey)
	if def == nil {
		return "", notiErrors.ErrTemplateNotFound
	}
	body, err := tplDomain.Render(def.Body, vars)
	if err != nil {
		return "", err
	}
	sid := "MOCK" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
	m.Sent = append(m.Sent, MockSent{
		GymID:         cfg.GymID,
		Recipient:     recipient,
		TemplateKey:   templateKey,
		Vars:          copyMap(vars),
		Body:          body,
		ProviderMsgID: sid,
		At:            time.Now().UTC(),
	})
	return sid, nil
}

func (m *MockProvider) SendFreeform(_ context.Context, cfg notiDomain.GymWhatsAppConfig, recipient, text string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.FailNextHard {
		m.FailNextHard = false
		return "", notiErrors.ErrSendFailedFinal
	}
	if m.FailNext > 0 {
		m.FailNext--
		return "", notiErrors.ErrSendFailedRetry
	}

	sid := "MOCK" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
	m.Sent = append(m.Sent, MockSent{
		GymID:         cfg.GymID,
		Recipient:     recipient,
		Body:          text,
		IsFreeform:    true,
		ProviderMsgID: sid,
		At:            time.Now().UTC(),
	})
	return sid, nil
}

func (m *MockProvider) RegisterSender(_ context.Context, phone string) (string, error) {
	if strings.TrimSpace(phone) == "" {
		return "", notiErrors.ErrInvalidPhone
	}
	return "MOCK_SENDER_" + phone, nil
}

// SentCount returns how many messages have been captured.
func (m *MockProvider) SentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Sent)
}

// LastSent returns a copy of the most recent send, or nil.
func (m *MockProvider) LastSent() *MockSent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Sent) == 0 {
		return nil
	}
	cp := m.Sent[len(m.Sent)-1]
	return &cp
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// StdoutProvider is the dev / sidecar fallback. Same idea as the mock but
// prints rendered output to logs instead of capturing — useful when running
// the cloud server locally without Twilio creds.
type StdoutProvider struct{}

func NewStdoutProvider() *StdoutProvider { return &StdoutProvider{} }

func (StdoutProvider) SendTemplate(_ context.Context, cfg notiDomain.GymWhatsAppConfig, recipient, templateKey string, vars map[string]string) (string, error) {
	def := tplDomain.LookupDefault(templateKey)
	body := templateKey
	if def != nil {
		if rendered, err := tplDomain.Render(def.Body, vars); err == nil {
			body = rendered
		}
	}
	log.Printf("[whatsapp/stdout] gym=%s to=%s template=%s body=%q", cfg.GymID, recipient, templateKey, body)
	return fmt.Sprintf("STDOUT-%d", time.Now().UnixNano()), nil
}

func (StdoutProvider) SendFreeform(_ context.Context, cfg notiDomain.GymWhatsAppConfig, recipient, text string) (string, error) {
	log.Printf("[whatsapp/stdout] gym=%s to=%s freeform=%q", cfg.GymID, recipient, text)
	return fmt.Sprintf("STDOUT-%d", time.Now().UnixNano()), nil
}

func (StdoutProvider) RegisterSender(_ context.Context, phone string) (string, error) {
	log.Printf("[whatsapp/stdout] register sender phone=%s", phone)
	return "STDOUT_SENDER", nil
}
