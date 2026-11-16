//go:build server

package controllers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type phoneCompleteFailureStub struct {
	err   error
	calls int
}

func (s *phoneCompleteFailureStub) Execute(context.Context, personalApp.CompletePhoneChangeInput) (personalApp.SessionTokens, error) {
	s.calls++
	return personalApp.SessionTokens{}, s.err
}

func TestPhoneChangeCompleteHTTPDistinguishesRejectedCodeFromUncertainEffect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := crypto.NewAccessTokens("phone_error", key, map[string]ed25519.PublicKey{"phone_error": pub})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	token, _, err := tokens.Issue(uuid.New(), uuid.New(), uuid.New(), now)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := personalMiddleware.NewPersonal(tokens, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		err          error
		status       int
		code, action string
		retryable    bool
	}{
		{"rejected-code", fmt.Errorf("wrapped: %w", personalErrors.ErrVerificationRejected), 409, "VERIFICATION_FAILED", "correct", false},
		{"uncertain-effect", personalErrors.ErrAuthAttemptUncertain, 503, "TEMPORARY_UNAVAILABLE", "retry", true},
		{"invalid-state", personalErrors.ErrAuthAttemptState, 409, "VERIFICATION_FAILED", "review", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &phoneCompleteFailureStub{err: tc.err}
			h := &PhoneChangeController{complete: stub}
			router := gin.New()
			router.POST("/me/phone-change/complete", auth, h.Complete)
			payload, err := json.Marshal(map[string]string{"attempt_id": uuid.NewString(), "code": "123456", "continuation_secret": strings.Repeat("a", 43)})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/me/phone-change/complete", strings.NewReader(string(payload)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Idempotency-Key", uuid.NewString())
			req.Header.Set("X-Device-Proof", strings.Repeat("b", 43))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			var body map[string]any
			if json.Unmarshal(response.Body.Bytes(), &body) != nil {
				t.Fatal("invalid response")
			}
			if stub.calls != 1 || response.Code != tc.status || body["code"] != tc.code || body["action"] != tc.action || body["retryable"] != tc.retryable {
				t.Fatalf("wrong failure classification: status=%d calls=%d code=%v action=%v", response.Code, stub.calls, body["code"], body["action"])
			}
			if response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "123456") || strings.Contains(response.Body.String(), strings.Repeat("a", 43)) {
				t.Fatal("unsafe failure response")
			}
		})
	}
}
