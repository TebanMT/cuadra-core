//go:build server

package controllers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type authResultResponseStub struct {
	result personalApp.PersonalAuthResult
}

func (s authResultResponseStub) Execute(context.Context, security.AccessClaims, uuid.UUID) (personalApp.PersonalAuthResult, error) {
	return s.result, nil
}

func TestPersonalAuthResultHTTPRejectsAmbiguousResultVariants(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := crypto.NewAccessTokens("result_union", key, map[string]ed25519.PublicKey{"result_union": pub})
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
	ack := &personalApp.Acknowledgement{OperationID: uuid.NewString(), Status: "completed", CompletedAt: now.Format("2006-01-02T15:04:05.000Z")}
	for _, tc := range []struct {
		name    string
		value   personalApp.PersonalAuthResult
		success bool
	}{
		{"ack", personalApp.PersonalAuthResult{Acknowledgement: ack}, true},
		{"empty", personalApp.PersonalAuthResult{}, false},
		{"ack-session", personalApp.PersonalAuthResult{Acknowledgement: ack, Session: &personalApp.SessionTokens{}}, false},
		{"ack-kit", personalApp.PersonalAuthResult{Acknowledgement: ack, RecoveryKit: &personalApp.RecoveryKit{}}, false},
		{"session-kit", personalApp.PersonalAuthResult{Session: &personalApp.SessionTokens{}, RecoveryKit: &personalApp.RecoveryKit{}}, false},
		{"all", personalApp.PersonalAuthResult{Acknowledgement: ack, Session: &personalApp.SessionTokens{}, RecoveryKit: &personalApp.RecoveryKit{}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewAuthResultController(authResultResponseStub{tc.value})
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.GET("/auth/results/:id", auth, h.Get)
			req := httptest.NewRequest("GET", "/auth/results/"+ack.OperationID, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			var body map[string]any
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("response contract")
			}
			if tc.success {
				if response.Code != 200 || len(body) != 3 || body["operation_id"] != ack.OperationID || body["status"] != ack.Status || body["completed_at"] != ack.CompletedAt {
					t.Fatal("ACK serialized as a wrapper or wrong variant")
				}
			} else if response.Code == 200 {
				t.Fatal("ambiguous result serialized")
			}
		})
	}
}
