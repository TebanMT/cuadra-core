//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The mobile-facing payload is signed independently from the server helper.
// In particular, an expiry at a whole second must retain its .000Z suffix.
func TestDeviceProofHTTPPostgresSignedContractReplayAndRevocation(t *testing.T) {
	d := finalizationDependencies(t)
	_, in, key := verifiedAccessFixture(t, d, accessTestPhone(), nil)
	finalize, err := personalApp.NewFinalizeAccess(d)
	if err != nil {
		t.Fatal(err)
	}
	signup, err := finalize.Execute(context.Background(), in)
	if err != nil || signup.Authorized == nil {
		t.Fatalf("signup failed: %v", err)
	}
	tokens := signup.Authorized.Session
	actor, err := d.Tokens.Verify(tokens.AccessToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	proofs := NewDeviceProofRepository()
	start, err := personalApp.NewStartDeviceProof(personalApp.StartDeviceProofDependencies{UoW: d.UoW, Sessions: d.Sessions, Proofs: proofs, Receipts: d.Receipts, Events: d.Events, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	complete, err := personalApp.NewCompleteDeviceProof(personalApp.CompleteDeviceProofDependencies{UoW: d.UoW, Sessions: d.Sessions, Proofs: proofs, Receipts: d.Receipts, Events: d.Events, Verifier: d.Proofs, Envelopes: d.Envelopes, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	http, err := controllers.NewDeviceProofController(start, complete)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := personalMiddleware.NewPersonal(d.Tokens, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	base := "/api/mancuerna/v1"
	router.POST(base+"/me/proofs", middleware, http.Start)
	router.POST(base+"/me/proofs/:id/complete", middleware, http.Complete)

	rotate, err := personalApp.NewRotateRecoveryKey(personalApp.RotateRecoveryKeyDependencies{UoW: d.UoW, Sessions: d.Sessions, Recovery: d.Recovery, Proofs: proofs, Receipts: d.Receipts, Events: d.Events, RecoverySecrets: d.RecoverySecrets, Envelopes: d.Envelopes, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	recoveryHTTP, err := controllers.NewRecoveryKeyController(rotate)
	if err != nil {
		t.Fatal(err)
	}
	router.POST(base+"/me/recovery-key", middleware, recoveryHTTP.Rotate)
	proofHeader := ""
	call := func(path, op string, body any) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", base+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		req.Header.Set("Idempotency-Key", op)
		if proofHeader != "" {
			req.Header.Set("X-Device-Proof", proofHeader)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("proof response cacheable")
		}
		var out map[string]any
		if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal("invalid JSON response")
		}
		return w.Code, out
	}
	proofID, op := uuid.NewString(), uuid.NewString()
	kitID, kitOp := uuid.NewString(), uuid.NewString()
	kitBody := map[string]any{"id": kitID}
	targetPayload, err := json.Marshal(map[string]any{"method": "POST", "path": "/me/recovery-key", "operation_id": kitOp, "body": kitBody})
	if err != nil {
		t.Fatal(err)
	}
	target := sha256.Sum256(targetPayload)
	body := map[string]any{"id": proofID, "purpose": "recovery_key", "request_hash": hex.EncodeToString(target[:])}
	code, proof := call("/me/proofs", op, body)
	if code != 200 || len(proof) != 5 || proof["id"] != proofID || proof["purpose"] != "recovery_key" || proof["request_hash"] != body["request_hash"] || !strings.HasSuffix(proof["expires_at"].(string), ".000Z") {
		t.Fatal("proof contract mismatch", code)
	}
	if code, replay := call("/me/proofs", op, body); code != 200 || !reflect.DeepEqual(proof, replay) {
		t.Fatal("proof start replay changed nonce/expiry", code)
	}
	body["Purpose"] = "delete_account"
	if code, _ := call("/me/proofs", uuid.NewString(), body); code != 400 {
		t.Fatal("case alias accepted", code)
	}
	delete(body, "Purpose")
	body["request_hash"] = strings.ToUpper(body["request_hash"].(string))
	if code, _ := call("/me/proofs", uuid.NewString(), body); code != 400 {
		t.Fatal("noncanonical hash accepted", code)
	}
	// encoding/json sorts these ASCII keys; values are all strings except v=1.
	payload, err := json.Marshal(map[string]any{"v": 1, "purpose": proof["purpose"], "account_id": actor.AccountID.String(), "session_id": actor.SessionID.String(), "device_id": actor.DeviceID.String(), "proof_id": proofID, "nonce": proof["nonce"], "request_hash": proof["request_hash"], "expires_at": proof["expires_at"]})
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
	completePath := "/me/proofs/" + proofID + "/complete"
	if code, _ := call(completePath, uuid.NewString(), map[string]any{"signature": strings.Repeat("A", 86)}); code != 403 {
		t.Fatal("invalid signature granted proof", code)
	}
	completeOp := uuid.NewString()
	code, result := call(completePath, completeOp, map[string]any{"signature": signature})
	if code != 200 || len(result) != 2 || result["expires_at"] != proof["expires_at"] || len(result["proof_token"].(string)) != 43 {
		t.Fatal("signed public payload rejected or token expiry extended", code)
	}
	if code, replay := call(completePath, completeOp, map[string]any{"signature": signature}); code != 200 || !reflect.DeepEqual(result, replay) {
		t.Fatal("proof completion retry changed token", code)
	}
	if code, _ := call(completePath, uuid.NewString(), map[string]any{"signature": signature}); code != 403 {
		t.Fatal("new operation reverified proof", code)
	}

	proofHeader = result["proof_token"].(string)
	code, kit := call("/me/recovery-key", kitOp, kitBody)
	if code != 200 || len(kit) != 3 || kit["id"] != kitID || kit["generation"] != "2" {
		t.Fatal("proof did not authorize rotation", code, kit["code"])
	}
	if code, retry := call("/me/recovery-key", kitOp, kitBody); code != 200 || !reflect.DeepEqual(kit, retry) {
		t.Fatal("rotation retry changed kit", code)
	}
	if code, _ := call("/me/recovery-key", uuid.NewString(), kitBody); code != 403 {
		t.Fatal("consumed proof reused", code)
	}
	if code, _ := call("/me/recovery-key", kitOp, map[string]any{"id": uuid.NewString()}); code != 409 {
		t.Fatal("changed rotation retry accepted", code)
	}
	proofHeader = ""
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		return d.Sessions.RevokeSession(tx, actor.AccountID, actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(completePath, completeOp, map[string]any{"signature": signature}); code != 401 {
		t.Fatal("revoked actor replayed proof token", code)
	}
}
