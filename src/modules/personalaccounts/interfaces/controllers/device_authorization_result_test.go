//go:build server

package controllers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type decisionResultReaderStub struct {
	calls int
	actor security.AccessClaims
	input personalApp.GetDeviceAuthorizationDecisionResultInput
}

func (s *decisionResultReaderStub) Execute(_ context.Context, actor security.AccessClaims, input personalApp.GetDeviceAuthorizationDecisionResultInput) (personalApp.DeviceAuthorizationDecisionResult, error) {
	s.calls++
	s.actor, s.input = actor, input
	return personalApp.DeviceAuthorizationDecisionResult{Status: personalApp.DeviceAuthorizationDecisionUnresolved}, nil
}

func TestDecisionResultHTTPBindsOriginalEffectAndRejectsAmbiguousQueries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := crypto.NewAccessTokens("result_test", key, map[string]ed25519.PublicKey{"result_test": pub})
	if err != nil {
		t.Fatal(err)
	}
	encoded, actor, err := tokens.Issue(uuid.New(), uuid.New(), uuid.New(), now)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := personalMiddleware.NewPersonal(tokens, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	stub := &decisionResultReaderStub{}
	handler, err := NewDeviceAuthorizationResultController(stub)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/me/device-authorizations/:id/decisions/:operation_id", auth, handler.Get)
	authorizationID, operationID, originalSession, originalDevice := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	path := "/me/device-authorizations/" + authorizationID.String() + "/decisions/" + operationID.String()
	q := url.Values{"original_session_id": {originalSession.String()}, "original_device_id": {originalDevice.String()}, "decision": {"approve"}}
	call := func(query, body, header string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest("GET", path+"?"+query, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+encoded)
		if header != "" {
			request.Header.Set(header, "synthetic-forbidden")
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("response must not be cached")
		}
		return response
	}
	for _, decision := range []string{"approve", "deny"} {
		q.Set("decision", decision)
		response := call(q.Encode(), "", "")
		if response.Code != 200 {
			t.Fatalf("valid query status %d", response.Code)
		}
		body, _ := json.Marshal(map[string]string{"decision": decision})
		hash, err := crypto.RequestHash("POST", "/me/device-authorizations/"+authorizationID.String()+"/decision", operationID, body)
		if err != nil {
			t.Fatal(err)
		}
		if stub.input.AuthorizationID != authorizationID || stub.input.OperationID != operationID || stub.input.OriginalSessionID != originalSession || stub.input.OriginalDeviceID != originalDevice || stub.input.RequestHash != hash || string(stub.input.Decision) != decision || stub.actor != actor {
			t.Fatal("original effect or live actor binding changed")
		}
	}
	valid := q.Encode()
	count := stub.calls
	for _, query := range []string{"", valid + "&decision=deny", valid + "&unknown=1", strings.ReplaceAll(valid, "decision=deny", "decision=APPROVE"), strings.ReplaceAll(valid, originalSession.String(), "invalid"), "decision=deny;original_session_id=x", valid + "&secret=ignored"} {
		if response := call(query, "", ""); response.Code == 200 {
			t.Fatalf("accepted ambiguous query %q", query)
		}
	}
	if response := call(valid, "{}", ""); response.Code != 400 {
		t.Fatalf("body status %d", response.Code)
	}
	for _, header := range []string{"X-Attempt-Secret", "X-Result-Secret", "X-Device-Proof", "Idempotency-Key"} {
		if response := call(valid, "", header); response.Code != 400 {
			t.Fatalf("mixed authority %s: %d", header, response.Code)
		}
	}
	if stub.calls != count {
		t.Fatal("invalid request reached reader")
	}
}
