//go:build server && mobilefixture

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureHoldCommands = "/api/mancuerna/v1/commands"
const fixtureHoldPush = "/api/mancuerna/v1/sync/push"

func TestMobileFixtureAuthorizationRouteIsClosedAndExact(t *testing.T) {
	const prefix = "/api/mancuerna/v1/me/device-authorizations/"
	const a = "a0000000-0000-4000-8000-000000000001"
	const b = "a0000000-0000-4000-8000-000000000002"
	for _, input := range []struct{ path, want string }{
		{prefix + a + "/decision", "decision"},
		{prefix + a + "/decisions/" + b, "result"},
		{prefix + a + "/decision/", ""}, {prefix + a + "/decision?extra=1", ""},
		{prefix + strings.ToUpper(a) + "/decision", ""}, {prefix + "00000000-0000-0000-0000-000000000000/decision", ""},
		{prefix + a + "/decisions/" + b + "/extra", ""}, {prefix + a + "/decisions/not-an-id", ""},
		{prefix + a + "/decisions", ""}, {"/api/mancuerna/v1/auth/device-authorizations/" + a + "/decision", ""},
	} {
		if got := mobileFixtureAuthorizationRoute(input.path); got != input.want {
			t.Fatalf("route %q got %q, want %q", input.path, got, input.want)
		}
	}
	r := httptest.NewRequest(http.MethodPost, prefix+a+"/decision", strings.NewReader(`{"decision":"approve"}`))
	trace, digest, err := mobileFixtureRequestHash(r)
	if !trace || err != nil || digest == "" {
		t.Fatal("decision must preserve opaque request evidence")
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil || string(raw) != `{"decision":"approve"}` {
		t.Fatal("tracing changed request body")
	}
	r = httptest.NewRequest(http.MethodGet, prefix+a+"/decisions/"+b, nil)
	if trace, _, _ := mobileFixtureRequestHash(r); trace {
		t.Fatal("result read must not be recorded as a submitted decision")
	}
}

func fixtureHoldRequest(path string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"private":"synthetic-body"}`))
	r.Header.Set("Idempotency-Key", "synthetic-operation")
	r.Header.Set("Authorization", "Bearer synthetic-credential-never-in-status")
	return r
}

func fixtureArmAndClaim(t *testing.T, h *mobileFixtureHolds) (mobileFixtureHoldStatus, *mobileFixtureHeldResponse) {
	t.Helper()
	armed, ok := h.arm(fixtureHoldPush, 10_000)
	if !ok {
		t.Fatal("could not arm fixture hold")
	}
	r := fixtureHoldRequest(fixtureHoldPush)
	_, bodyHash, err := mobileFixtureRequestHash(r)
	if err != nil {
		t.Fatal("could not hash synthetic request")
	}
	held := h.claim(r, http.StatusOK, bodyHash, mobileFixtureDigest([]byte("synthetic-response")))
	if held == nil {
		t.Fatal("could not claim fixture hold")
	}
	return armed, held
}

func TestMobileFixtureHoldRequiresPost200AndExactPath(t *testing.T) {
	h := &mobileFixtureHolds{}
	armed, ok := h.arm(fixtureHoldPush, 10_000)
	if !ok || armed.State != "armed" {
		t.Fatal("hold must start armed")
	}
	for _, test := range []struct {
		method string
		path   string
		status int
	}{{http.MethodGet, fixtureHoldPush, 200}, {http.MethodPost, fixtureHoldCommands, 200}, {http.MethodPost, fixtureHoldPush, 401}, {http.MethodPost, fixtureHoldPush, 201}} {
		r := httptest.NewRequest(test.method, test.path, nil)
		if h.claim(r, test.status, "", "") != nil || h.snapshot()[0].State != "armed" {
			t.Fatal("nonmatching response consumed hold")
		}
	}
	if h.release(armed.ID) {
		t.Fatal("release must not acknowledge a response that has not arrived")
	}
	if h.claim(fixtureHoldRequest(fixtureHoldPush), 200, "body-hash", "response-hash") == nil {
		t.Fatal("matching response was not held")
	}
	if h.claim(fixtureHoldRequest(fixtureHoldPush), 200, "second", "second") != nil {
		t.Fatal("one hold must not retain a second response")
	}
	if _, ok := h.arm(fixtureHoldCommands, 10_000); ok {
		t.Fatal("a second route must not arm concurrently")
	}
	if h.release("different-id") || h.snapshot()[0].State != "waiting" {
		t.Fatal("unrelated release changed active hold")
	}
	h.stop()
}

func TestMobileFixtureHoldReleaseDoesNotChangeResponseIdentity(t *testing.T) {
	h := &mobileFixtureHolds{}
	armed, held := fixtureArmAndClaim(t, h)
	before := h.snapshot()[0]
	done := make(chan bool, 1)
	go func() { done <- h.wait(context.Background(), held) }()
	select {
	case <-done:
		t.Fatal("held response escaped before release")
	default:
	}
	if !h.release(armed.ID) || !h.release(armed.ID) {
		t.Fatal("same release should be idempotent")
	}
	select {
	case send := <-done:
		if !send {
			t.Fatal("released response should be forwarded")
		}
	case <-time.After(time.Second):
		t.Fatal("release did not wake request")
	}
	after := h.snapshot()[0]
	after.State = before.State
	if after != before {
		t.Fatal("release modified recorded response identity")
	}
	if _, ok := h.arm(fixtureHoldCommands, 10_000); !ok {
		t.Fatal("completed hold should permit another observation")
	}
	h.stop()
}

func TestMobileFixtureHoldCancellationExpirationAndStopDoNotForward(t *testing.T) {
	for _, reason := range []string{"cancelled", "expired", "stopped"} {
		t.Run(reason, func(t *testing.T) {
			h := &mobileFixtureHolds{}
			armed, held := fixtureArmAndClaim(t, h)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch reason {
			case "cancelled":
				cancel()
			case "expired":
				h.mu.Lock()
				held.deadline = time.Now().Add(-time.Second)
				h.mu.Unlock()
			case "stopped":
				h.stop()
			}
			if h.wait(ctx, held) || h.snapshot()[0].State != reason || h.release(armed.ID) {
				t.Fatal("terminated hold must not authorize forwarding")
			}
			if reason == "stopped" {
				if _, ok := h.arm(fixtureHoldCommands, 10_000); ok {
					t.Fatal("stopped fixture must not arm new work")
				}
			}
		})
	}
}

func TestMobileFixtureHoldExpiredArmAndBoundedHistory(t *testing.T) {
	h := &mobileFixtureHolds{}
	for _, invalid := range []struct {
		path string
		ms   int
	}{{"/api/mancuerna/v1/auth/check", 1000}, {fixtureHoldCommands, 99}, {fixtureHoldCommands, 60_001}} {
		if _, ok := h.arm(invalid.path, invalid.ms); ok {
			t.Fatal("invalid hold admitted")
		}
	}
	for i := 0; i < 64; i++ {
		if _, ok := h.arm(fixtureHoldCommands, 1000); !ok {
			t.Fatal("bounded history rejected valid observation")
		}
		h.mu.Lock()
		h.entries[i].deadline = time.Now().Add(-time.Second)
		h.mu.Unlock()
		if h.active(fixtureHoldCommands) || h.snapshot()[i].State != "expired" {
			t.Fatal("unused arm must expire")
		}
	}
	if _, ok := h.arm(fixtureHoldCommands, 1000); ok {
		t.Fatal("history capacity must fail without deleting evidence")
	}
}

func TestMobileFixtureHoldStatusContainsOnlyCopiesAndOpaqueHashes(t *testing.T) {
	h := &mobileFixtureHolds{}
	_, _ = fixtureArmAndClaim(t, h)
	snapshot := h.snapshot()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"synthetic-credential", "synthetic-body", "synthetic-response", "synthetic-operation", "Authorization", "authorization"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("status exposed private request or response content")
		}
	}
	if len(snapshot[0].BodyHash) != 64 || len(snapshot[0].OperationIDHash) != 64 || len(snapshot[0].ResponseHash) != 64 {
		t.Fatal("status must contain exact opaque digests")
	}
	if _, err := time.Parse(time.RFC3339Nano, snapshot[0].DeadlineAt); err != nil {
		t.Fatal("status requires explicit UTC deadline")
	}
	snapshot[0].State = "tampered"
	if h.snapshot()[0].State != "waiting" {
		t.Fatal("status must not expose mutable hold state")
	}
	h.stop()
}

func TestMobileFixtureHoldConcurrentFinishAndRead(t *testing.T) {
	h := &mobileFixtureHolds{}
	armed, held := fixtureArmAndClaim(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(5)
	go func() { defer workers.Done(); h.wait(ctx, held) }()
	go func() { defer workers.Done(); h.release(armed.ID) }()
	go func() { defer workers.Done(); h.stop() }()
	go func() { defer workers.Done(); cancel() }()
	go func() { defer workers.Done(); h.snapshot() }()
	workers.Wait()
	state := h.snapshot()[0].State
	if state != "released" && state != "stopped" && state != "cancelled" {
		t.Fatal("concurrent termination left a live hold")
	}
}

func TestMobileFixtureRequestHashPreservesExactPushBytes(t *testing.T) {
	for _, path := range []string{fixtureHoldPush, fixtureHoldCommands, "/api/mancuerna/v1/auth/recovery/complete"} {
		raw := "{ \"operations\": [],\n \"protocol_version\": 1 }"
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(raw))
		trace, digest, err := mobileFixtureRequestHash(r)
		if err != nil || !trace || digest != mobileFixtureDigest([]byte(raw)) {
			t.Fatal("exact request bytes were not hashed")
		}
		restored, err := io.ReadAll(r.Body)
		if err != nil || string(restored) != raw {
			t.Fatal("tracing changed request before the actual Go handler")
		}
	}
	oversized := httptest.NewRequest(http.MethodPost, fixtureHoldPush, strings.NewReader(strings.Repeat("x", 2*1024*1024+1)))
	if _, _, err := mobileFixtureRequestHash(oversized); err == nil {
		t.Fatal("oversized tracing request must be rejected")
	}
	if trace, _, err := mobileFixtureRequestHash(httptest.NewRequest(http.MethodGet, fixtureHoldPush, nil)); err != nil || trace {
		t.Fatal("tracing must not admit other methods")
	}
}

func TestMobileFixtureHoldControlRejectsUnknownTrailingAndOversizedJSON(t *testing.T) {
	for _, raw := range []string{`{"id":"x","extra":1}`, `{"id":"x"}{}`, `{"id":"` + strings.Repeat("x", 1100) + `"}`, `null`, `[]`} {
		var body struct {
			ID string `json:"id"`
		}
		w := httptest.NewRecorder()
		ok := mobileFixtureControlBody(w, httptest.NewRequest(http.MethodPost, "/release", strings.NewReader(raw)), &body)
		// null decodes as the zero-value object but cannot match a generated ID.
		if raw == "null" {
			if !ok || (&mobileFixtureHolds{}).release(body.ID) {
				t.Fatal("null must not release a response")
			}
		} else if ok || w.Code != http.StatusBadRequest {
			t.Fatal("malformed control body accepted")
		}
	}
}
