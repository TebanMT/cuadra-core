// Package accesswebhook provides an outbound webhook fired whenever a
// checkin resolves to "access allowed". This is the differentiator that
// lets a barrio gym wire its OWN turnstile / electric lock / camera — Cuadra
// emits the signal, the gym (or its electrician) bridges it to whatever
// hardware they bought.
//
// The webhook URL + optional shared secret live in `gyms.kiosk_settings`
// (json column) — operator edits them from settings UI, no DB migration.
//
// Delivery posture:
//   - fire-and-forget goroutine (caller's request returns immediately)
//   - HMAC-SHA256 signature in X-Cuadra-Signature header (when secret set)
//   - 5s timeout per attempt, 1 quick retry on transient errors
//   - failures log + drop (the kiosk MUST NOT block on a flaky relay)
package accesswebhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Event is the JSON body posted to the configured URL.
type Event struct {
	Schema       string    `json:"schema"`
	GymID        uuid.UUID `json:"gym_id"`
	MemberID     uuid.UUID `json:"member_id"`
	MemberName   string    `json:"member_name"`
	Method       string    `json:"method"`
	Result       string    `json:"result"` // "allowed_active" | "allowed_expiring_soon"
	OccurredAt   time.Time `json:"occurred_at"`
}

// Dispatcher is what callers (checkin controller) talk to. The interface is
// trivially small so tests can supply a no-op or a recording fake.
type Dispatcher interface {
	OnAccessAllowed(ctx context.Context, ev Event)
}

// HTTPDispatcher is the production impl backed by net/http. Construct with
// NewHTTPDispatcher(...) at process bootstrap.
type HTTPDispatcher struct {
	UoW    sharedDomain.UnitOfWork
	Gyms   gymRepo.GymRepository
	Client *http.Client
}

func NewHTTPDispatcher(uow sharedDomain.UnitOfWork, gyms gymRepo.GymRepository) *HTTPDispatcher {
	return &HTTPDispatcher{
		UoW:    uow,
		Gyms:   gyms,
		Client: &http.Client{Timeout: 5 * time.Second},
	}
}

// OnAccessAllowed is non-blocking. It spawns a goroutine that resolves the
// gym's webhook URL and posts the event. We intentionally don't await the
// result — kiosk feedback latency is the priority.
func (d *HTTPDispatcher) OnAccessAllowed(ctx context.Context, ev Event) {
	if ev.GymID == uuid.Nil {
		return
	}
	go d.deliver(ev)
}

func (d *HTTPDispatcher) deliver(ev Event) {
	url, secret := d.lookupSettings(ev.GymID)
	if url == "" {
		return
	}
	body, err := json.Marshal(ev)
	if err != nil {
		log.Printf("accesswebhook: marshal event: %v", err)
		return
	}
	for attempt := 1; attempt <= 2; attempt++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			log.Printf("accesswebhook: build request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "cuadra-sidecar/1")
		if secret != "" {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(body)
			req.Header.Set("X-Cuadra-Signature", hex.EncodeToString(mac.Sum(nil)))
		}
		resp, err := d.Client.Do(req)
		if err != nil {
			if attempt == 1 {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			log.Printf("accesswebhook: %s: %v", url, err)
			return
		}
		// Drain body so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return
		}
		// Non-2xx: only retry on 5xx.
		if resp.StatusCode >= 500 && attempt == 1 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		log.Printf("accesswebhook: %s returned HTTP %d", url, resp.StatusCode)
		return
	}
}

// lookupSettings resolves (url, secret) from gyms.kiosk_settings. Errors
// silently drop — never block kiosk on a misconfigured webhook.
func (d *HTTPDispatcher) lookupSettings(gymID uuid.UUID) (string, string) {
	tx, err := d.UoW.Query(context.Background())
	if err != nil {
		return "", ""
	}
	gym, err := d.Gyms.GetByID(tx, gymID)
	if err != nil || gym == nil || gym.KioskSettings == nil {
		return "", ""
	}
	return readStr(gym.KioskSettings, "access_webhook_url"),
		readStr(gym.KioskSettings, "access_webhook_secret")
}

func readStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// noopDispatcher is what tests / processes that don't want webhook traffic
// inject. Cloud uses it (cloud doesn't kiosk).
type noopDispatcher struct{}

func (noopDispatcher) OnAccessAllowed(_ context.Context, _ Event) {}

// Noop returns a Dispatcher that drops every event silently. Useful for
// the cloud server (which doesn't drive a turnstile) and for tests.
func Noop() Dispatcher { return noopDispatcher{} }

// ResultIsAllowed returns true for the two "allowed_*" access statuses the
// checkin module emits. Centralised here so the controller can decide
// whether to dispatch without importing the access package.
func ResultIsAllowed(status string) bool {
	switch status {
	case "allowed_active", "allowed_expiring_soon":
		return true
	}
	return false
}

// fmt + import smoke (keep these symbols referenced in case future logging
// switches from log.Printf to fmt-driven destinations).
var _ = fmt.Sprint
