//go:build server && integration

package repositories

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestDeletionHTTPPostgresProofLimitedResultAndPolicyGate(t *testing.T) {
	f := newPhoneChangeFixture(t, 20)
	dependencies := deletionTestDependencies(f)
	deleteHTTP, err := controllers.NewDeletionController(deletionTestUseCase(t, dependencies))
	if err != nil {
		t.Fatal(err)
	}
	dependencies.Policy = nil
	disabledHTTP, err := controllers.NewDeletionController(deletionTestUseCase(t, dependencies))
	if err != nil {
		t.Fatal(err)
	}
	baseDependencies := finalizationDependencies(t)
	personalResult, err := personalApp.NewGetPersonalAuthResult(personalApp.GetPersonalAuthResultDependencies{UoW: f.d.Phone.UoW, Sessions: f.d.Sessions, Results: NewPersonalAuthResultRepository(), Recovery: baseDependencies.Recovery, RecoverySecrets: baseDependencies.RecoverySecrets, Envelopes: f.d.Envelopes, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	resultHTTP, err := controllers.NewAuthResultControllerWithDeletion(personalResult, deletionTestReader(t, f))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := personalMiddleware.NewPersonal(f.d.Tokens, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	resultAccess, err := personalMiddleware.NewResultAccess(auth)
	if err != nil {
		t.Fatal(err)
	}
	proofHTTP, err := controllers.NewDeviceProofController(proofTestStart(t, f.startProof), proofTestComplete(t, f.completeProof))
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	const base = "/api/mancuerna/v1"
	router.POST(base+"/me/deletion", auth, deleteHTTP.Request)
	router.POST(base+"/test/policy-disabled", auth, disabledHTTP.Request)
	router.POST(base+"/me/proofs", auth, proofHTTP.Start)
	router.POST(base+"/me/proofs/:id/complete", auth, proofHTTP.Complete)
	router.GET(base+"/auth/results/:id", resultAccess, resultHTTP.Get)
	call := func(method, path, token, op, proof, secret string, body any) (int, map[string]any) {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, base+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if op != "" {
			req.Header.Set("Idempotency-Key", op)
		}
		if proof != "" {
			req.Header.Set("X-Device-Proof", proof)
		}
		if secret != "" {
			req.Header.Set("X-Result-Secret", secret)
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		var value map[string]any
		if out.Header().Get("Cache-Control") != "no-store" || out.Header().Get("Pragma") != "no-cache" || json.Unmarshal(out.Body.Bytes(), &value) != nil {
			t.Fatal("invalid deletion response envelope")
		}
		return out.Code, value
	}
	op := uuid.NewString()
	resultSecret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{12}, 32))
	body := map[string]any{"confirmation": "delete_my_account", "policy_version": "synthetic-test-only", "result_secret": resultSecret}
	canonical, _ := json.Marshal(map[string]any{"method": "POST", "path": "/me/deletion", "operation_id": op, "body": body})
	hash := sha256.Sum256(canonical)
	proofID := uuid.NewString()
	code, challenge := call("POST", "/me/proofs", f.tokens.AccessToken, uuid.NewString(), "", "", map[string]any{"id": proofID, "purpose": "delete_account", "request_hash": hex.EncodeToString(hash[:])})
	if code != 200 {
		t.Fatal("proof start status", code)
	}
	payload, _ := json.Marshal(map[string]any{"v": 1, "purpose": challenge["purpose"], "account_id": f.actor.AccountID.String(), "session_id": f.actor.SessionID.String(), "device_id": f.actor.DeviceID.String(), "proof_id": proofID, "nonce": challenge["nonce"], "request_hash": challenge["request_hash"], "expires_at": challenge["expires_at"]})
	code, proof := call("POST", "/me/proofs/"+proofID+"/complete", f.tokens.AccessToken, uuid.NewString(), "", "", map[string]any{"signature": base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.key, payload))})
	if code != 200 {
		t.Fatal("proof complete status", code)
	}
	proofToken := proof["proof_token"].(string)
	code, failure := call("POST", "/test/policy-disabled", f.tokens.AccessToken, op, proofToken, "", body)
	if code != 503 || failure["code"] != "POLICY_NOT_CONFIGURED" {
		t.Fatal("unconfigured deletion not closed", code)
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.device_proofs WHERE id=? AND consumed_at IS NULL`, proofID) != 1 {
		t.Fatal("policy gate consumed proof")
	}
	code, _ = call("POST", "/me/deletion", f.tokens.AccessToken, op, proofToken, resultSecret, body)
	if code != 400 {
		t.Fatal("accepted result header in deletion POST", code)
	}
	code, ack := call("POST", "/me/deletion", f.tokens.AccessToken, op, proofToken, "", body)
	if code != 200 || len(ack) != 3 || ack["status"] != "deletion_pending" || ack["operation_id"] != op {
		t.Fatal("deletion acceptance failed", code)
	}
	code, replay := call("GET", "/auth/results/"+op, "", "", "", resultSecret, nil)
	if code != 200 || !reflect.DeepEqual(ack, replay) {
		t.Fatal("lost response not recovered by exact limited capability", code)
	}
	for _, tc := range []struct {
		path, token, secret string
		body                any
		status              int
	}{
		{"/auth/results/" + op, "", "", nil, 401},
		{"/auth/results/" + op, f.tokens.AccessToken, resultSecret, nil, 400},
		{"/auth/results/" + op, f.tokens.AccessToken, "", nil, 401},
		{"/auth/results/" + uuid.NewString(), "", resultSecret, nil, 404},
		{"/auth/results/" + op, "", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{13}, 32)), nil, 404},
		{"/auth/results/" + op + "?secret=forbidden", "", resultSecret, nil, 400},
		{"/auth/results/" + op, "", resultSecret, map[string]any{}, 400},
	} {
		code, response := call("GET", tc.path, tc.token, "", "", tc.secret, tc.body)
		if code != tc.status {
			t.Fatalf("result authority status=%d expected=%d", code, tc.status)
		}
		if response["access_token"] != nil || response["recovery_kit"] != nil || response["data_key"] != nil {
			t.Fatal("result capability exposed credentials")
		}
	}
	code, _ = call("POST", "/me/deletion", f.tokens.AccessToken, op, proofToken, "", body)
	if code != 401 {
		t.Fatal("revoked JWT replay unexpectedly accepted", code)
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=? AND purpose='delete_account'`, op) != 1 {
		t.Fatal("duplicate deletion receipt")
	}
}
