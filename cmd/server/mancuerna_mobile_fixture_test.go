//go:build server && mobilefixture

package main

// This test-only binary uses the actual mounted API, Twilio adapter, JWTs and
// PostgreSQL. Its transport has no network delegate: all Verify responses are
// synthetic. Nothing here is compiled into the application server.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type mobileFixtureTransport struct {
	mu        sync.Mutex
	config    mancuernaConfig
	resources map[string]map[string]any
	counts    map[string]int
}

// Response holds belong only to this synthetic fixture. A released response is
// permission to forward the original bytes, never proof that the client saved
// its receipt. No request/response body or credential is retained in status.
type mobileFixtureHoldStatus struct {
	ID              string `json:"id"`
	Path            string `json:"path"`
	State           string `json:"state"`
	DeadlineAt      string `json:"deadline_at"`
	Status          int    `json:"status,omitempty"`
	OperationIDHash string `json:"operation_id_hash,omitempty"`
	BodyHash        string `json:"body_hash,omitempty"`
	ResponseHash    string `json:"response_hash,omitempty"`
}

type mobileFixtureHeldResponse struct {
	status   mobileFixtureHoldStatus
	deadline time.Time
	done     chan struct{}
}

type mobileFixtureHolds struct {
	mu      sync.Mutex
	entries []*mobileFixtureHeldResponse
	stopped bool
}

func mobileFixtureHoldPath(path string) bool {
	return path == "/api/mancuerna/v1/commands" || path == "/api/mancuerna/v1/sync/push"
}

// Dynamic authorization routes remain exact synthetic-fixture targets. A
// decision fault cannot apply to another authorization or its result reader.
func mobileFixtureAuthorizationRoute(path string) string {
	const prefix = "/api/mancuerna/v1/me/device-authorizations/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	validID := func(value string) bool {
		id, err := uuid.Parse(value)
		return err == nil && id != uuid.Nil && id.String() == value && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
	}
	if len(parts) == 2 && validID(parts[0]) && parts[1] == "decision" {
		return "decision"
	}
	if len(parts) == 3 && validID(parts[0]) && parts[1] == "decisions" && validID(parts[2]) {
		return "result"
	}
	return ""
}

func mobileFixtureDigest(raw []byte) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func mobileFixtureRequestHash(r *http.Request) (bool, string, error) {
	trace := r.Method == http.MethodPost && (mobileFixtureHoldPath(r.URL.Path) || mobileFixtureAuthorizationRoute(r.URL.Path) == "decision" || r.URL.Path == "/api/mancuerna/v1/auth/recovery/complete")
	if !trace {
		return false, "", nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 {
		return false, "", errors.New("fixture request invalid")
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return true, mobileFixtureDigest(raw), nil
}

func (h *mobileFixtureHolds) finishLocked(entry *mobileFixtureHeldResponse, state string) {
	if entry.status.State == "armed" || entry.status.State == "waiting" {
		entry.status.State = state
		close(entry.done)
	}
}

func (h *mobileFixtureHolds) activeLocked() *mobileFixtureHeldResponse {
	if len(h.entries) == 0 {
		return nil
	}
	entry := h.entries[len(h.entries)-1]
	if !time.Now().Before(entry.deadline) {
		h.finishLocked(entry, "expired")
	}
	if entry.status.State == "armed" || entry.status.State == "waiting" {
		return entry
	}
	return nil
}

func (h *mobileFixtureHolds) arm(path string, timeoutMS int) (mobileFixtureHoldStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !mobileFixtureHoldPath(path) || timeoutMS < 100 || timeoutMS > 60_000 || h.stopped || h.activeLocked() != nil || len(h.entries) >= 64 {
		return mobileFixtureHoldStatus{}, false
	}
	deadline := time.Now().Add(time.Duration(timeoutMS) * time.Millisecond)
	entry := &mobileFixtureHeldResponse{status: mobileFixtureHoldStatus{ID: uuid.NewString(), Path: path, State: "armed", DeadlineAt: deadline.UTC().Format(time.RFC3339Nano)}, deadline: deadline, done: make(chan struct{})}
	h.entries = append(h.entries, entry)
	return entry.status, true
}

func (h *mobileFixtureHolds) active(path string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.activeLocked()
	return entry != nil && entry.status.Path == path
}

func (h *mobileFixtureHolds) claim(r *http.Request, status int, requestHash, responseHash string) *mobileFixtureHeldResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.activeLocked()
	if entry == nil || entry.status.State != "armed" || r.Method != http.MethodPost || entry.status.Path != r.URL.Path || status != http.StatusOK {
		return nil
	}
	entry.status.State = "waiting"
	entry.status.Status = status
	entry.status.OperationIDHash = mobileFixtureDigest([]byte(r.Header.Get("Idempotency-Key")))
	entry.status.BodyHash = requestHash
	entry.status.ResponseHash = responseHash
	return entry
}

func (h *mobileFixtureHolds) release(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activeLocked()
	if len(h.entries) == 0 {
		return false
	}
	entry := h.entries[len(h.entries)-1]
	if entry.status.ID != id || (entry.status.State != "waiting" && entry.status.State != "released") {
		return false
	}
	h.finishLocked(entry, "released")
	return true
}

func (h *mobileFixtureHolds) wait(ctx context.Context, entry *mobileFixtureHeldResponse) bool {
	timer := time.NewTimer(time.Until(entry.deadline))
	defer timer.Stop()
	reason := ""
	select {
	case <-entry.done:
	case <-ctx.Done():
		reason = "cancelled"
	case <-timer.C:
		reason = "expired"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if reason != "" {
		h.finishLocked(entry, reason)
	}
	return entry.status.State == "released" && ctx.Err() == nil
}

func (h *mobileFixtureHolds) snapshot() []mobileFixtureHoldStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activeLocked()
	result := make([]mobileFixtureHoldStatus, len(h.entries))
	for i, entry := range h.entries {
		result[i] = entry.status
	}
	return result
}

func (h *mobileFixtureHolds) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	if entry := h.activeLocked(); entry != nil {
		h.finishLocked(entry, "stopped")
	}
}

func mobileFixtureControlBody(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	return true
}

func mobileFixtureCloseResponse(w http.ResponseWriter) {
	if hijacker, ok := w.(http.Hijacker); ok {
		if connection, _, err := hijacker.Hijack(); err == nil {
			_ = connection.Close()
		}
	}
}

func (p *mobileFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	user, password, ok := r.BasicAuth()
	base := "/v2/Services/" + p.config.Verify.ServiceSID
	if !ok || user != p.config.Verify.Username || password != p.config.Verify.Password || r.URL.Scheme != "https" || r.URL.Host != "verify.twilio.com" || !strings.HasPrefix(r.URL.Path, base+"/") {
		p.counts["blocked_network_requests"]++
		return nil, errors.New("fixture rejects all external requests")
	}
	var raw []byte
	if r.Body != nil {
		var err error
		raw, err = io.ReadAll(io.LimitReader(r.Body, 8192))
		if err != nil {
			return nil, errors.New("fixture request invalid")
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err := r.ParseForm(); err != nil {
		return nil, errors.New("fixture form invalid")
	}
	path := strings.TrimPrefix(r.URL.Path, base)
	var resource map[string]any
	switch {
	case r.Method == "POST" && path == "/Verifications":
		if r.Form.Get("Channel") != "sms" || (r.Form.Get("To") != "+525555559901" && r.Form.Get("To") != "+525555559902") {
			p.counts["blocked_destination_requests"]++
			return nil, errors.New("only synthetic fixture destinations accepted")
		}
		sid := "VE" + strings.ReplaceAll(uuid.NewString(), "-", "")
		resource = map[string]any{"sid": sid, "account_sid": p.config.Verify.AccountSID, "service_sid": p.config.Verify.ServiceSID, "to": r.Form.Get("To"), "channel": "sms", "status": "pending", "date_created": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)}
		p.resources[sid] = resource
		p.counts["synthetic_starts"]++
	case r.Method == "POST" && path == "/VerificationCheck":
		resource = p.resources[r.Form.Get("VerificationSid")]
		if resource != nil && r.Form.Get("Code") == "123456" {
			resource["status"] = "approved"
		}
		p.counts["synthetic_checks"]++
	case r.Method == "GET" && strings.HasPrefix(path, "/Verifications/"):
		resource = p.resources[strings.TrimPrefix(path, "/Verifications/")]
		p.counts["synthetic_fetches"]++
	default:
		return nil, errors.New("fixture endpoint rejected")
	}
	code := 200
	if resource == nil {
		code = 404
		resource = map[string]any{"code": 20404}
	}
	body, _ := json.Marshal(resource)
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
}

func TestMancuernaMobileFixture(t *testing.T) {
	dir := os.Getenv("MANCUERNA_MOBILE_FIXTURE_DIR")
	info, err := os.Stat(dir)
	if err != nil || !filepath.IsAbs(dir) || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("explicit private fixture directory required")
	}
	c, err := readMancuernaConfig(filepath.Join(dir, "config.json"))
	if err != nil || c.Verify.AccountSID != "AC"+strings.Repeat("0", 32) || c.Verify.ServiceSID != "VA"+strings.Repeat("0", 32) || c.Verify.Password != strings.Repeat("a", 32) || c.LocalVerifyGuard == nil {
		t.Fatal("only synthetic fixture configuration accepted")
	}
	for _, name := range mancuernaLocalPGVariables {
		if os.Getenv(name) != "" {
			t.Fatal("ambient database configuration rejected")
		}
	}
	c.AuthDatabaseURL, err = localPersonalDSN(c.AuthDatabaseURL)
	if err != nil {
		t.Fatal("invalid fixture auth DSN")
	}
	c.DataDatabaseURL, err = localPersonalDSN(c.DataDatabaseURL)
	if err != nil {
		t.Fatal("invalid fixture data DSN")
	}
	provider := &mobileFixtureTransport{config: c, resources: map[string]map[string]any{}, counts: map[string]int{}}
	previous := http.DefaultTransport
	http.DefaultTransport = provider
	defer func() { http.DefaultTransport = previous }()
	gin.SetMode(gin.TestMode)
	router := newServerRouter()
	cleanup, err := mountMancuerna(router, c)
	if err != nil {
		t.Fatal("fixture mount failed")
	}
	defer cleanup()
	var mu sync.Mutex
	drop := map[string]int{}
	unauthorized := map[string]int{}
	calls := map[string]int{}
	responses := map[string]map[int]int{}
	traces := []map[string]any{}
	holds := &mobileFixtureHolds{}
	defer holds.stop()
	paths := map[string]bool{"/api/mancuerna/v1/sync/push": true, "/api/mancuerna/v1/auth/start": true, "/api/mancuerna/v1/auth/check": true, "/api/mancuerna/v1/auth/refresh": true, "/api/mancuerna/v1/auth/logout": true, "/api/mancuerna/v1/auth/recovery/complete": true, "/api/mancuerna/v1/me": true, "/api/mancuerna/v1/me/recovery-key/confirm": true, "/api/mancuerna/v1/commands": true, "/api/mancuerna/v1/sync/pull": true, "/api/mancuerna/v1/sync/snapshots": true, "/api/mancuerna/v1/me/vaults": true}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counted := paths[r.URL.Path] || mobileFixtureAuthorizationRoute(r.URL.Path) != ""
		// Only opaque hashes enter the private control evidence. The native
		// journeys can compare exact replays without exporting kit/JWT/body.
		trace, requestHash, err := mobileFixtureRequestHash(r)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		if counted {
			calls[r.URL.Path]++
		}
		force := unauthorized[r.URL.Path] > 0
		if force {
			unauthorized[r.URL.Path]--
		}
		mu.Unlock()
		if force {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(401)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "AUTH_REQUIRED", "message": "Synthetic expired access", "retryable": false, "request_id": uuid.NewString(), "action": "authenticate", "fields": []any{}})
			return
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, r)
		mu.Lock()
		if trace {
			traces = append(traces, map[string]any{"path": r.URL.Path, "operation_id_hash": mobileFixtureDigest([]byte(r.Header.Get("Idempotency-Key"))), "body_hash": requestHash, "authorization_hash": mobileFixtureDigest([]byte(r.Header.Get("Authorization"))), "response_hash": mobileFixtureDigest(response.Body.Bytes()), "status": response.Code})
		}
		if counted {
			if responses[r.URL.Path] == nil {
				responses[r.URL.Path] = map[int]int{}
			}
			responses[r.URL.Path][response.Code]++
		}
		lose := drop[r.URL.Path] > 0 && response.Code == 200
		if lose {
			drop[r.URL.Path]--
		}
		var held *mobileFixtureHeldResponse
		if !lose {
			held = holds.claim(r, response.Code, requestHash, mobileFixtureDigest(response.Body.Bytes()))
		}
		mu.Unlock()
		// The actual API/transaction has finished. Do not hold the control or
		// provider mutex while the native runner observes UI and releases it.
		if lose || (held != nil && !holds.wait(r.Context(), held)) {
			mobileFixtureCloseResponse(w)
			return
		}
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	})
	listener, err := net.Listen("tcp", "127.0.0.1:18884")
	if err != nil {
		t.Fatal("fixture loopback port unavailable")
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	controlPath := filepath.Join(dir, "fixture-control.sock")
	controlListener, err := net.Listen("unix", controlPath)
	if err != nil {
		t.Fatal("fixture control unavailable")
	}
	if os.Chmod(controlPath, 0600) != nil {
		t.Fatal("fixture control permissions")
	}
	defer os.Remove(controlPath)
	stop := make(chan struct{}, 1)
	control := http.NewServeMux()
	control.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		provider.mu.Lock()
		defer provider.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "running", "scope": "synthetic Verify only; real API and PostgreSQL", "api_calls": calls, "api_response_statuses": responses, "provider": provider.counts, "request_traces": traces, "response_holds": holds.snapshot()})
	})
	control.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		holds.stop()
		select {
		case stop <- struct{}{}:
		default:
		}
		w.WriteHeader(204)
	})
	setFault := func(auth bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Path  string `json:"path"`
				Count int    `json:"count"`
			}
			if !mobileFixtureControlBody(w, r, &body) {
				return
			}
			if (!paths[body.Path] && mobileFixtureAuthorizationRoute(body.Path) != "decision") || body.Count < 1 || body.Count > 2 || (auth && body.Path != "/api/mancuerna/v1/me" && body.Path != "/api/mancuerna/v1/me/recovery-key/confirm") {
				w.WriteHeader(400)
				return
			}
			mu.Lock()
			if !auth && holds.active(body.Path) {
				mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				return
			}
			if auth {
				unauthorized[body.Path] = body.Count
			} else {
				drop[body.Path] = body.Count
			}
			mu.Unlock()
			w.WriteHeader(204)
		}
	}
	control.HandleFunc("POST /drop", setFault(false))
	control.HandleFunc("POST /auth-required", setFault(true))
	control.HandleFunc("POST /hold", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path      string `json:"path"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if !mobileFixtureControlBody(w, r, &body) {
			return
		}
		if !mobileFixtureHoldPath(body.Path) || body.TimeoutMS < 100 || body.TimeoutMS > 60_000 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if drop[body.Path] > 0 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		status, ok := holds.arm(body.Path, body.TimeoutMS)
		if !ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})
	control.HandleFunc("POST /release", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		if !mobileFixtureControlBody(w, r, &body) {
			return
		}
		if !holds.release(body.ID) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	controlServer := &http.Server{Handler: control, ReadHeaderTimeout: 2 * time.Second}
	defer controlServer.Close()
	go func() { _ = controlServer.Serve(controlListener) }()
	if os.WriteFile(filepath.Join(dir, "fixture-ready"), []byte("synthetic Verify; loopback18884\n"), 0600) != nil {
		t.Fatal("fixture readiness marker failed")
	}
	defer os.Remove(filepath.Join(dir, "fixture-ready"))
	select {
	case <-stop:
	case err := <-finished:
		if err != http.ErrServerClosed {
			t.Fatal("fixture server stopped unexpectedly")
		}
	case <-time.After(25 * time.Minute):
		t.Fatal("fixture deadline elapsed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
