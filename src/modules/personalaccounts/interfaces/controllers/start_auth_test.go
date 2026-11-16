//go:build server

package controllers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type startCapture struct {
	input   personalApp.StartAuthInput
	calls   int
	failure error
}

func (s *startCapture) Execute(_ context.Context, in personalApp.StartAuthInput) (*attempt.AuthAttempt, error) {
	s.calls++
	s.input = in
	if s.failure != nil {
		return nil, s.failure
	}
	nonce := [32]byte{1}
	return attempt.New(attempt.NewAttempt{Binding: in.Binding, ProposedAccountID: in.ProposedAccountID, Candidate: in.Candidate, Nonce: nonce, Purpose: in.Purpose, DestinationE164: in.DestinationE164, Channel: in.Channel, RequestHash: in.RequestHash}, controllerNow, 10*time.Minute)
}

var controllerNow = time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)

func validStartJSON(t *testing.T) []byte {
	t.Helper()
	value := map[string]any{"attempt_id": uuid.NewString(), "phone": "+525555555555", "country": "MX", "channel": "sms", "purpose": "access", "candidate": map[string]any{"id": uuid.NewString(), "platform": "android", "label": "Synthetic phone", "public_key": base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))}, "new_account_id": uuid.NewString(), "continuation_secret": strings.Repeat("s", 43)}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func invokeStart(t *testing.T, raw []byte, s *startCapture, change func(*httptest.ResponseRecorder, *gin.Engine)) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h, err := NewStartAuthController(s, func() time.Time { return controllerNow }, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	router.POST("/api/mancuerna/v1/auth/start", h.Start)
	w := httptest.NewRecorder()
	if change != nil {
		change(w, router)
		return w
	}
	req := httptest.NewRequest("POST", "/api/mancuerna/v1/auth/start", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.RemoteAddr = "127.0.0.1:54321"
	router.ServeHTTP(w, req)
	return w
}
func TestStartAuthHTTPMapsOnlyPublicAttemptAndTrustedOrigin(t *testing.T) {
	s := &startCapture{}
	w := invokeStart(t, validStartJSON(t), s, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" || s.calls != 1 || s.input.Origin != "127.0.0.1" {
		t.Fatal("cache/origin policy not applied")
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response) != 6 || response["destination_masked"] != "•••• 5555" || response["state"] != "pending" {
		t.Fatal("public attempt shape changed")
	}
	for _, forbidden := range []string{"+525555555555", "provider_reference", "continuation_hash", strings.Repeat("s", 43), "request_hash", "candidate"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatal("private attempt data leaked")
		}
	}
}
func TestStartAuthHTTPRejectsAmbiguousRequestsBeforeExecution(t *testing.T) {
	valid := validStartJSON(t)
	for name, raw := range map[string][]byte{
		"duplicate": append([]byte(`{"phone":"+525555555554",`), valid[1:]...),
		"unknown":   append([]byte(`{"gym_id":"ignored",`), valid[1:]...),
		"trailing":  append(append([]byte(nil), valid...), []byte(` {}`)...),
		"null":      []byte(`null`),
		"secret":    bytes.Replace(valid, []byte(strings.Repeat("s", 43)), []byte("short"), 1),
		"country":   bytes.Replace(valid, []byte(`"MX"`), []byte(`"mx"`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			s := &startCapture{}
			w := invokeStart(t, raw, s, nil)
			if w.Code != 400 || s.calls != 0 {
				t.Fatal("ambiguous request reached use case")
			}
		})
	}
	s := &startCapture{}
	w := invokeStart(t, nil, s, func(w *httptest.ResponseRecorder, r *gin.Engine) {
		req := httptest.NewRequest("POST", "/api/mancuerna/v1/auth/start", bytes.NewReader(valid))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Add("Idempotency-Key", uuid.NewString())
		req.Header.Add("Idempotency-Key", uuid.NewString())
		r.ServeHTTP(w, req)
	})
	if w.Code != 400 || s.calls != 0 {
		t.Fatal("ambiguous retry header accepted")
	}
}
func TestStartAuthHTTPErrorEnvelopeNeverIncludesInternalError(t *testing.T) {
	s := &startCapture{failure: personalErrors.ErrBudgetExceeded}
	w := invokeStart(t, validStartJSON(t), s, nil)
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 429 || response["code"] != "RATE_LIMITED" || response["action"] != "wait" || response["fields"] == nil {
		t.Fatal("contract error envelope mismatch")
	}
	if _, err := uuid.Parse(response["request_id"].(string)); err != nil {
		t.Fatal("request id missing")
	}
}
