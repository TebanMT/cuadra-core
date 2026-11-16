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
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Real HTTP parsing, JWT, public proof signatures, restricted PostgreSQL and
// encrypted receipts; Verify delivery remains an explicit synthetic provider.
func TestPhoneChangeHTTPPostgresProofBodyBindingAndLostResponse(t *testing.T) {
	f := newPhoneChangeFixture(t, 20)
	providerAt := time.Now().UTC().Truncate(time.Millisecond)
	f.p.start = func(_ context.Context, in verification.StartRequest) (verification.Result, error) {
		return verification.Result{Reference: "VE-phone-http-synthetic", Channel: in.Channel, State: verification.Pending, CreatedAt: providerAt, ExpiresAt: providerAt.Add(10 * time.Minute)}, nil
	}
	f.p.check = func(_ context.Context, in verification.CheckRequest) (verification.Result, error) {
		return verification.Result{Reference: in.Reference, Channel: verification.SMS, State: verification.Approved, CreatedAt: providerAt, ExpiresAt: providerAt.Add(10 * time.Minute)}, nil
	}
	now := time.Now
	f.d.Phone.Now = func() time.Time { return now() }
	phoneHTTP, err := controllers.NewPhoneChangeController(phoneChangeStart(t, f.d), phoneChangeComplete(t, f.d), time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	proofHTTP, err := controllers.NewDeviceProofController(proofTestStart(t, f.startProof), proofTestComplete(t, f.completeProof))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := personalMiddleware.NewPersonal(f.d.Tokens, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	const base = "/api/mancuerna/v1"
	router.POST(base+"/me/phone-change/start", auth, phoneHTTP.Start)
	router.POST(base+"/me/phone-change/complete", auth, phoneHTTP.Complete)
	router.POST(base+"/me/proofs", auth, proofHTTP.Start)
	router.POST(base+"/me/proofs/:id/complete", auth, proofHTTP.Complete)
	baseDependencies := finalizationDependencies(t)
	getResult, err := personalApp.NewGetPersonalAuthResult(personalApp.GetPersonalAuthResultDependencies{UoW: f.d.Phone.UoW, Sessions: f.d.Sessions, Results: NewPersonalAuthResultRepository(), Recovery: baseDependencies.Recovery, RecoverySecrets: baseDependencies.RecoverySecrets, Envelopes: f.d.Envelopes, Now: func() time.Time { return now() }})
	if err != nil {
		t.Fatal(err)
	}
	resultHTTP, err := controllers.NewAuthResultController(getResult)
	if err != nil {
		t.Fatal(err)
	}
	router.GET(base+"/auth/results/:id", auth, resultHTTP.Get)
	phoneAttemptDependencies := personalApp.PhoneChangeAttemptDependencies{Phone: f.d, Resolver: NewPhoneChangeAttemptResolver(), Policy: personalApp.PhoneRetryPolicy{ProviderTimeout: 20 * time.Second, ReconcileAfter: 25 * time.Second, ResendCooldown: 30 * time.Second}}
	phoneGet, err := personalApp.NewGetPhoneChangeAttempt(phoneAttemptDependencies)
	if err != nil {
		t.Fatal(err)
	}
	phoneResend, err := personalApp.NewResendPhoneChange(phoneAttemptDependencies)
	if err != nil {
		t.Fatal(err)
	}
	checkPhone, err := personalApp.NewCheckPhoneVerification(f.d.Phone, 5)
	if err != nil {
		t.Fatal(err)
	}
	finalize, err := personalApp.NewFinalizeAccess(baseDependencies)
	if err != nil {
		t.Fatal(err)
	}
	checkAccess, err := personalApp.NewCheckAccess(checkPhone, finalize)
	if err != nil {
		t.Fatal(err)
	}
	attemptHTTP, err := controllers.NewAccessController(checkAccess, phoneGet, phoneResend, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	router.GET(base+"/auth/attempts/:id", attemptHTTP.Get)
	router.POST(base+"/auth/attempts/:id/resend", attemptHTTP.Resend)
	readAttempt := func(attemptID, secret string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest("GET", base+"/auth/attempts/"+attemptID, nil)
		req.Header.Set("X-Attempt-Secret", secret)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var out map[string]any
		if w.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("attempt response invalid")
		}
		if out["session"] != nil || out["recovery_kit"] != nil {
			t.Fatal("limited attempt exposed account authority")
		}
		return w.Code, out
	}
	readResult := func(bearer, op string, extra map[string][]string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest("GET", base+"/auth/results/"+op, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		for k, v := range extra {
			req.Header[k] = v
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var out map[string]any
		if w.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("result response invalid")
		}
		return w.Code, out
	}
	call := func(bearer, path, op, proof string, body any, extra map[string][]string) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", base+path, bytes.NewReader(raw))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("Idempotency-Key", op)
		if proof != "" {
			req.Header.Set("X-Device-Proof", proof)
		}
		for k, v := range extra {
			req.Header[k] = v
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cacheable phone change")
		}
		var out map[string]any
		if json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("invalid response JSON")
		}
		if w.Code != 200 && (out["access_token"] != nil || out["refresh_token"] != nil) {
			t.Fatal("error exposed credentials")
		}
		return w.Code, out
	}
	proofFor := func(path, op string, body any) string {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"method": "POST", "path": path, "operation_id": op, "body": body})
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(payload)
		proofID := uuid.NewString()
		code, proof := call(f.tokens.AccessToken, "/me/proofs", uuid.NewString(), "", map[string]any{"id": proofID, "purpose": "phone_change", "request_hash": hex.EncodeToString(hash[:])}, nil)
		if code != 200 {
			t.Fatal("proof start", code)
		}
		payload, err = json.Marshal(map[string]any{"v": 1, "purpose": proof["purpose"], "account_id": f.actor.AccountID.String(), "session_id": f.actor.SessionID.String(), "device_id": f.actor.DeviceID.String(), "proof_id": proofID, "nonce": proof["nonce"], "request_hash": proof["request_hash"], "expires_at": proof["expires_at"]})
		if err != nil {
			t.Fatal(err)
		}
		signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.key, payload))
		code, token := call(f.tokens.AccessToken, "/me/proofs/"+proofID+"/complete", uuid.NewString(), "", map[string]any{"signature": signature}, nil)
		if code != 200 {
			t.Fatal("proof complete", code)
		}
		return token["proof_token"].(string)
	}
	const startPath = "/me/phone-change/start"
	const completePath = "/me/phone-change/complete"
	startOp, attemptID, secret, destination := uuid.NewString(), uuid.NewString(), strings.Repeat("s", 43), accessTestPhone()
	body := map[string]any{"attempt_id": attemptID, "phone": destination, "country": "MX", "channel": "sms", "continuation_secret": secret}
	if code, _ := call("", startPath, startOp, "", body, nil); code != 401 {
		t.Fatal("anonymous start", code)
	}
	if code, _ := call(f.tokens.AccessToken, startPath, startOp, "", body, nil); code != 403 {
		t.Fatal("missing proof", code)
	}
	proof := proofFor(startPath, startOp, body)
	if code, _ := call(f.tokens.AccessToken, startPath, startOp, proof, body, map[string][]string{"X-Attempt-Secret": {secret}}); code != 400 {
		t.Fatal("anonymous secret alias", code)
	}
	body["Phone"] = destination
	if code, _ := call(f.tokens.AccessToken, startPath, startOp, proof, body, nil); code != 400 {
		t.Fatal("case alias", code)
	}
	delete(body, "Phone")
	if f.p.starts.Load() != 0 {
		t.Fatal("invalid HTTP reached provider")
	}
	code, started := call(f.tokens.AccessToken, startPath, startOp, proof, body, nil)
	if code != 200 || len(started) != 6 || started["id"] != attemptID || started["state"] != "pending" {
		t.Fatal("phone start contract", code)
	}
	if code, replay := call(f.tokens.AccessToken, startPath, startOp, proof, body, nil); code != 200 || replay["nonce"] != started["nonce"] || replay["expires_at"] != started["expires_at"] {
		t.Fatal("start replay", code)
	}
	if code, state := readAttempt(attemptID, secret); code != 200 || len(state) != 2 || state["status"] != "pending" || state["attempt"].(map[string]any)["state"] != "pending" {
		t.Fatal("limited phone status", code)
	}
	if code, _ := readAttempt(attemptID, strings.Repeat("x", 43)); code != 404 {
		t.Fatal("foreign attempt secret", code)
	}
	if code, _ := call("", "/auth/attempts/"+attemptID+"/resend", uuid.NewString(), "", map[string]any{"channel": "sms"}, map[string][]string{"X-Attempt-Secret": {secret}}); code != 429 {
		t.Fatal("phone resend ignored cooldown", code)
	}
	completeOp := uuid.NewString()
	completeBody := map[string]any{"attempt_id": attemptID, "code": "123456", "continuation_secret": secret}
	completeProof := proofFor(completePath, completeOp, completeBody)
	if code, _ := call(f.tokens.AccessToken, completePath, completeOp, proof, completeBody, nil); code != 403 {
		t.Fatal("start proof authorized completion", code)
	}
	completeBody["continuation_secret"] = strings.Repeat("z", 43)
	if code, _ := call(f.tokens.AccessToken, completePath, completeOp, completeProof, completeBody, nil); code != 404 {
		t.Fatal("foreign body secret", code)
	}
	completeBody["continuation_secret"] = secret
	code, result := call(f.tokens.AccessToken, completePath, completeOp, completeProof, completeBody, nil)
	if code != 200 || len(result) != 9 || result["account_id"] != f.tokens.AccountID || result["session_id"] != f.tokens.SessionID || result["device_id"] != f.tokens.DeviceID || result["generation"] != "2" {
		t.Fatal("phone session contract", code, result["code"])
	}
	if code, replay := call(f.tokens.AccessToken, completePath, completeOp, completeProof, completeBody, nil); code != 200 || !reflect.DeepEqual(result, replay) {
		t.Fatal("lost completion response diverged", code)
	}
	if code, replay := readResult(f.tokens.AccessToken, completeOp, nil); code != 200 || !reflect.DeepEqual(result, replay) {
		t.Fatal("GET committed result diverged", code)
	}
	if code, state := readAttempt(attemptID, secret); code != 200 || len(state) != 2 || state["status"] != "pending" || state["attempt"].(map[string]any)["state"] != "consumed" {
		t.Fatal("consumed phone attempt exposed session or changed view", code)
	}
	if code, _ := readResult("", completeOp, nil); code != 401 {
		t.Fatal("anonymous result granted", code)
	}
	for _, header := range []string{"X-Attempt-Secret", "X-Result-Secret"} {
		if code, _ := readResult(f.tokens.AccessToken, completeOp, map[string][]string{header: {secret}}); code != 400 {
			t.Fatal("mixed result authority accepted", code)
		}
	}
	if code, _ := readResult(f.tokens.AccessToken, startOp, nil); code != 404 {
		t.Fatal("start receipt exposed as session", code)
	}
	completeBody["code"] = "654321"
	if code, _ := call(f.tokens.AccessToken, completePath, completeOp, completeProof, completeBody, nil); code != 409 {
		t.Fatal("changed completion body", code)
	}
	completeBody["code"] = "123456"
	if f.p.starts.Load() != 1 || f.p.checks.Load() != 1 || f.p.fetches.Load() != 0 {
		t.Fatal("HTTP replay duplicated provider calls")
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.accounts WHERE id=? AND phone_e164=? AND security_version=2 AND revision=1`, f.actor.AccountID, destination) != 1 {
		t.Fatal("phone change did not commit once")
	}
	now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	if code, result := readResult(f.tokens.AccessToken, completeOp, nil); code != 410 || result["code"] != "RESULT_EXPIRED" {
		t.Fatal("GET result TTL extended", code)
	}
	if code, result := call(f.tokens.AccessToken, completePath, completeOp, completeProof, completeBody, nil); code != 410 || result["code"] != "RESULT_EXPIRED" {
		t.Fatal("result window extended", code)
	}
	now = time.Now
	if err = f.d.Phone.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, f.actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(f.tokens.AccessToken, completePath, completeOp, completeProof, completeBody, nil); code != 401 {
		t.Fatal("revoked actor replayed session", code)
	}
	if code, _ := readResult(f.tokens.AccessToken, completeOp, nil); code != 401 {
		t.Fatal("revoked actor read result", code)
	}
	if code, _ := readAttempt(attemptID, secret); code != 401 {
		t.Fatal("revoked initiator retained attempt capability", code)
	}
}
