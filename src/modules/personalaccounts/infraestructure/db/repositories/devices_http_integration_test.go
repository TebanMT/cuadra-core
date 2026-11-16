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
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Real JWT, proof signatures and restricted PostgreSQL; devices are authorized
// fixtures here. This does not certify the separate candidate-approval flow.
func TestDevicesHTTPPostgresPublicProofExactRevokeAndAll(t *testing.T) {
	start, complete, _, actor, key := proofTestFixture(t)
	other := devicesTestAdd(t, start, actor)
	tokens := finalizationDependencies(t).Tokens
	mint := func(who security.AccessClaims) string {
		t.Helper()
		token, _, err := tokens.Issue(who.AccountID, who.SessionID, who.DeviceID, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	bearer, otherBearer := mint(actor), mint(other)
	h, err := controllers.NewDevicesController(devicesTestList(t, start, time.Now), devicesTestRevoke(t, devicesTestDependencies(start)))
	if err != nil {
		t.Fatal(err)
	}
	proofHTTP, err := controllers.NewDeviceProofController(proofTestStart(t, start), proofTestComplete(t, complete))
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := personalMiddleware.NewPersonal(tokens, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	const base = "/api/mancuerna/v1"
	router.GET(base+"/me/devices", middleware, h.List)
	router.POST(base+"/me/devices/:id/revoke", middleware, h.RevokeOne)
	router.POST(base+"/me/devices/revoke", middleware, h.RevokeScope)
	router.POST(base+"/me/proofs", middleware, proofHTTP.Start)
	router.POST(base+"/me/proofs/:id/complete", middleware, proofHTTP.Complete)
	call := func(method, token, path, op, proof string, body any) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, base+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if op != "" {
			req.Header.Set("Idempotency-Key", op)
		}
		if proof != "" {
			req.Header.Set("X-Device-Proof", proof)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("device response cacheable")
		}
		var result map[string]any
		if json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal("invalid response")
		}
		return w.Code, result
	}
	freshProof := func(path, op string, body any) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"method": "POST", "path": path, "operation_id": op, "body": body})
		hash := sha256.Sum256(raw)
		proofID := uuid.NewString()
		code, challenge := call("POST", bearer, "/me/proofs", uuid.NewString(), "", map[string]any{"id": proofID, "purpose": "revoke_devices", "request_hash": hex.EncodeToString(hash[:])})
		if code != 200 {
			t.Fatal("proof start", code)
		}
		payload, _ := json.Marshal(map[string]any{"v": 1, "purpose": "revoke_devices", "account_id": actor.AccountID.String(), "session_id": actor.SessionID.String(), "device_id": actor.DeviceID.String(), "proof_id": proofID, "nonce": challenge["nonce"], "request_hash": challenge["request_hash"], "expires_at": challenge["expires_at"]})
		code, result := call("POST", bearer, "/me/proofs/"+proofID+"/complete", uuid.NewString(), "", map[string]any{"signature": base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))})
		if code != 200 {
			t.Fatal("proof completion", code)
		}
		return result["proof_token"].(string)
	}
	if code, body := call("GET", bearer, "/me/devices", "", "", nil); code != 200 || len(body) != 2 || len(body["items"].([]any)) != 2 {
		t.Fatal("list projection", code)
	}
	if code, _ := call("GET", bearer, "/me/devices?limit=1&limit=2", "", "", nil); code != 400 {
		t.Fatal("ambiguous list query", code)
	}
	path := "/me/devices/" + other.DeviceID.String() + "/revoke"
	op := uuid.NewString()
	empty := map[string]any{}
	if code, _ := call("POST", bearer, path, op, "", empty); code != 403 {
		t.Fatal("proofless revocation", code)
	}
	proof := freshProof(path, op, empty)
	code, ack := call("POST", bearer, path, op, proof, empty)
	if code != 200 || len(ack) != 3 || ack["operation_id"] != op {
		t.Fatal("revoke acknowledgement", code)
	}
	code, retry := call("POST", bearer, path, op, proof, empty)
	if code != 200 || retry["completed_at"] != ack["completed_at"] {
		t.Fatal("revoke replay", code)
	}
	if code, _ := call("POST", bearer, path, uuid.NewString(), proof, empty); code != 403 {
		t.Fatal("proof reused for another operation", code)
	}
	if code, body := call("GET", otherBearer, "/me/devices", "", "", nil); code != 401 || body["code"] != "SESSION_REVOKED" {
		t.Fatal("revoked JWT remained usable", code)
	}
	if code, _ := call("GET", bearer, "/me/devices", "", "", nil); code != 200 {
		t.Fatal("untargeted actor revoked", code)
	}
	path = "/me/devices/revoke"
	op = uuid.NewString()
	body := map[string]any{"scope": "all"}
	proof = freshProof(path, op, body)
	if code, _ := call("POST", bearer, path, op, proof, map[string]any{"scope": "others"}); code != 403 {
		t.Fatal("proof body binding ignored", code)
	}
	if code, _ := call("POST", bearer, path, op, proof, body); code != 200 {
		t.Fatal("all revocation", code)
	}
	if code, result := call("POST", bearer, path, op, proof, body); code != 401 || result["code"] != "SESSION_REVOKED" {
		t.Fatal("lost self-revoke response bypassed retired session", code)
	}
}
