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
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Gin parsing, JWT, public canonical proof payload, real restricted PostgreSQL,
// envelope encryption and HTTP output. Signup starts from a verified synthetic
// attempt; no provider delivery or local mobile file encryption is asserted.
func TestVaultHTTPPostgresPublicProofReplayAndActorIsolation(t *testing.T) {
	d := finalizationDependencies(t)
	finalize, err := personalApp.NewFinalizeAccess(d)
	if err != nil {
		t.Fatal(err)
	}
	newActor := func() (personalApp.SessionTokens, security.AccessClaims, ed25519.PrivateKey) {
		t.Helper()
		_, in, key := verifiedAccessFixture(t, d, accessTestPhone(), nil)
		result, err := finalize.Execute(context.Background(), in)
		if err != nil || result.Authorized == nil {
			t.Fatal("synthetic signup failed")
		}
		tokens := result.Authorized.Session
		claims, err := d.Tokens.Verify(tokens.AccessToken, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tokens, claims, key
	}
	tokens, actor, private := newActor()
	proofs := NewDeviceProofRepository()
	startD := personalApp.StartDeviceProofDependencies{UoW: d.UoW, Sessions: d.Sessions, Proofs: proofs, Receipts: d.Receipts, Events: d.Events, Now: time.Now}
	completeD := personalApp.CompleteDeviceProofDependencies{UoW: d.UoW, Sessions: d.Sessions, Proofs: proofs, Receipts: d.Receipts, Events: d.Events, Verifier: d.Proofs, Envelopes: d.Envelopes, Now: time.Now}
	proofHTTP, err := controllers.NewDeviceProofController(proofTestStart(t, startD), proofTestComplete(t, completeD))
	if err != nil {
		t.Fatal(err)
	}
	createD, unlockD := vaultTestDependencies(startD, completeD)
	vaultHTTP, err := controllers.NewVaultController(vaultTestCreate(t, createD), vaultTestUnlock(t, unlockD))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := personalMiddleware.NewPersonal(d.Tokens, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	const base = "/api/mancuerna/v1"
	router.POST(base+"/me/vaults", auth, vaultHTTP.Create)
	router.POST(base+"/me/vaults/:id/unlock", auth, vaultHTTP.Unlock)
	router.POST(base+"/me/proofs", auth, proofHTTP.Start)
	router.POST(base+"/me/proofs/:id/complete", auth, proofHTTP.Complete)
	call := func(bearer, path, op, proof string, body any) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", base+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Idempotency-Key", op)
		if proof != "" {
			req.Header.Set("X-Device-Proof", proof)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("sensitive HTTP response is cacheable")
		}
		var result map[string]any
		if json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatal("invalid HTTP JSON")
		}
		if response.Code != 200 && (result["data_key"] != nil || result["vault"] != nil) {
			t.Fatal("failure leaked vault material")
		}
		return response.Code, result
	}
	// Compute both canonical messages from public HTTP data, independently of
	// the backend's RequestHash and DeviceProofPayload helpers.
	freshProof := func(bearer string, who security.AccessClaims, key ed25519.PrivateKey, path, effectOp string) string {
		t.Helper()
		request, err := json.Marshal(map[string]any{"method": "POST", "path": path, "operation_id": effectOp, "body": map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		target := sha256.Sum256(request)
		proofID := uuid.NewString()
		code, challenge := call(bearer, "/me/proofs", uuid.NewString(), "", map[string]any{"id": proofID, "purpose": "vault_unlock", "request_hash": hex.EncodeToString(target[:])})
		if code != 200 || len(challenge) != 5 || challenge["purpose"] != "vault_unlock" || challenge["request_hash"] != hex.EncodeToString(target[:]) {
			t.Fatal("vault proof challenge did not match contract", code)
		}
		payload, err := json.Marshal(map[string]any{"v": 1, "purpose": challenge["purpose"], "account_id": who.AccountID.String(), "session_id": who.SessionID.String(), "device_id": who.DeviceID.String(), "proof_id": proofID, "nonce": challenge["nonce"], "request_hash": challenge["request_hash"], "expires_at": challenge["expires_at"]})
		if err != nil {
			t.Fatal(err)
		}
		signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
		code, token := call(bearer, "/me/proofs/"+proofID+"/complete", uuid.NewString(), "", map[string]any{"signature": signature})
		if code != 200 || len(token) != 2 || token["expires_at"] != challenge["expires_at"] {
			t.Fatal("public vault proof signature rejected", code)
		}
		proof, ok := token["proof_token"].(string)
		if !ok || len(proof) != 43 {
			t.Fatal("invalid proof token output")
		}
		return proof
	}
	id, createOp := uuid.NewString(), uuid.NewString()
	body := map[string]any{"id": id}
	code, created := call(tokens.AccessToken, "/me/vaults", createOp, "", body)
	if code != 200 || len(created) != 3 || created["id"] != id || created["key_version"] != "test" {
		t.Fatal("vault creation HTTP contract mismatch", code)
	}
	dataKey, ok := created["data_key"].(string)
	if !ok || len(dataKey) != 43 {
		t.Fatal("vault data key not base64url256")
	}
	if decoded, err := base64.RawURLEncoding.Strict().DecodeString(dataKey); err != nil || len(decoded) != 32 {
		t.Fatal("vault key encoding invalid")
	}
	if code, replay := call(tokens.AccessToken, "/me/vaults", createOp, "", body); code != 200 || !reflect.DeepEqual(replay, created) {
		t.Fatal("vault create retry changed key", code)
	}
	if code, _ := call(tokens.AccessToken, "/me/vaults", createOp, "", map[string]any{"id": uuid.NewString()}); code != 409 {
		t.Fatal("changed create retry accepted", code)
	}
	if code, _ := call(tokens.AccessToken, "/me/vaults", uuid.NewString(), "", body); code != 409 {
		t.Fatal("new operation replaced existing vault", code)
	}
	path, unlockOp := "/me/vaults/"+id+"/unlock", uuid.NewString()
	if code, _ := call(tokens.AccessToken, path, unlockOp, "", map[string]any{}); code != 403 {
		t.Fatal("vault unlocked without fresh proof", code)
	}
	proof := freshProof(tokens.AccessToken, actor, private, path, unlockOp)
	if code, _ := call(tokens.AccessToken, path, unlockOp, proof, map[string]any{"unexpected": true}); code != 400 {
		t.Fatal("altered unlock body accepted", code)
	}
	if code, _ := call(tokens.AccessToken, path, uuid.NewString(), proof, map[string]any{}); code != 403 {
		t.Fatal("proof authorized another operation", code)
	}
	code, unlocked := call(tokens.AccessToken, path, unlockOp, proof, map[string]any{})
	if code != 200 || !reflect.DeepEqual(unlocked, created) {
		t.Fatal("unlock did not recover exact original key", code)
	}
	if code, replay := call(tokens.AccessToken, path, unlockOp, proof, map[string]any{}); code != 200 || !reflect.DeepEqual(replay, created) {
		t.Fatal("unlock retry changed key", code)
	}
	otherTokens, other, otherPrivate := newActor()
	otherOp := uuid.NewString()
	otherProof := freshProof(otherTokens.AccessToken, other, otherPrivate, path, otherOp)
	if code, _ := call(otherTokens.AccessToken, path, otherOp, otherProof, map[string]any{}); code != 404 {
		t.Fatal("another account recovered vault", code)
	}
	if code, _ := call(otherTokens.AccessToken, path, unlockOp, proof, map[string]any{}); code != 404 {
		t.Fatal("another account recovered unlock receipt", code)
	}
	second := authSessionTestSession(t, actor.AccountID, actor.DeviceID)
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error { return d.Sessions.CreateSession(tx, actor.AccountID, second) }); err != nil {
		t.Fatal(err)
	}
	secondToken, _, err := d.Tokens.Issue(actor.AccountID, second.ID, actor.DeviceID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := call(secondToken, path, unlockOp, proof, map[string]any{}); code != 403 {
		t.Fatal("same-account other session recovered receipt", code)
	}
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		return d.Sessions.RevokeSession(tx, actor.AccountID, actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(tokens.AccessToken, path, unlockOp, proof, map[string]any{}); code != 401 {
		t.Fatal("revoked actor recovered cached key", code)
	}
	if code, _ := call(tokens.AccessToken, "/me/vaults", createOp, "", body); code != 401 {
		t.Fatal("revoked actor replayed create", code)
	}
}
