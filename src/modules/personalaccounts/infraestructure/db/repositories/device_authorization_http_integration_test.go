//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	hashing "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/budget"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Actual HTTP controllers, signed public proof payloads and restricted
// PostgreSQL. Only OTP delivery/check is simulated; this does not certify
// Twilio delivery, a physical mobile device or its persistent key storage.
func TestDeviceAuthorizationHTTPPostgresCandidateOnlyResultAndActorIsolation(t *testing.T) {
	f := authorizationTestFixture(t)
	bearer, _, err := f.d.Tokens.Issue(f.actor.AccountID, f.actor.SessionID, f.actor.DeviceID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	provider := &phoneProviderDouble{}
	providerAt := time.Now().UTC().Truncate(time.Millisecond)
	provider.start = func(_ context.Context, in verification.StartRequest) (verification.Result, error) {
		return verification.Result{Reference: "VE-authorization-http-synthetic", Channel: in.Channel, State: verification.Pending, CreatedAt: providerAt, ExpiresAt: providerAt.Add(10 * time.Minute)}, nil
	}
	provider.check = func(_ context.Context, in verification.CheckRequest) (verification.Result, error) {
		return verification.Result{Reference: in.Reference, Channel: verification.SMS, State: verification.Approved, CreatedAt: providerAt, ExpiresAt: providerAt.Add(10 * time.Minute)}, nil
	}
	rules, err := budget.NewRules(map[budget.Scope]budget.Limit{budget.Destination: {Window: time.Hour, MaxSend: 10, MaxCheck: 20}, budget.Origin: {Window: time.Hour, MaxSend: 10, MaxCheck: 20}, budget.Service: {Window: time.Hour, MaxSend: 10, MaxCheck: 20}})
	if err != nil {
		t.Fatal(err)
	}
	budgetKey := sha256.Sum256([]byte(uuid.NewString()))
	subjects, err := hashing.NewSubjects(budgetKey[:])
	if err != nil {
		t.Fatal(err)
	}
	phone := personalApp.StartAuthDependencies{UoW: f.d.UoW, Attempts: f.d.Attempts, Receipts: f.d.Receipts, Budgets: NewOTPBudgetRepository(), Rules: rules, Subjects: subjects, Provider: provider, ServiceIdentity: uuid.NewString(), ProviderValidity: 10 * time.Minute, Now: time.Now}
	start, err := personalApp.NewStartAuth(phone)
	if err != nil {
		t.Fatal(err)
	}
	check, err := personalApp.NewCheckPhoneVerification(phone, 5)
	if err != nil {
		t.Fatal(err)
	}
	finalize, err := personalApp.NewFinalizeAccess(f.d)
	if err != nil {
		t.Fatal(err)
	}
	access, err := personalApp.NewCheckAccess(check, finalize)
	if err != nil {
		t.Fatal(err)
	}
	read, err := personalApp.NewGetAttempt(personalApp.GetAttemptDependencies{UoW: f.d.UoW, Attempts: f.d.Attempts, Provider: provider, Now: time.Now, ProviderTimeout: 20 * time.Second, ReconcileAfter: 25 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	recoveryStatus, err := personalApp.NewGetRecoveryStatus(personalApp.GetRecoveryStatusDependencies{UoW: f.d.UoW, Attempts: f.d.Attempts, Recovery: f.d.Recovery, Sessions: f.d.Sessions, Receipts: f.d.Receipts, RecoverySecrets: f.d.RecoverySecrets, Envelopes: f.d.Envelopes, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	status, err := personalApp.NewGetAuthStatus(read, finalize, authorizationTestStatus(t, f.status), recoveryStatus)
	if err != nil {
		t.Fatal(err)
	}
	resend, err := personalApp.NewResendAuth(phone, personalApp.PhoneRetryPolicy{ProviderTimeout: 20 * time.Second, ReconcileAfter: 25 * time.Second, ResendCooldown: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	startHTTP, err := controllers.NewStartAuthController(start, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	accessHTTP, err := controllers.NewAccessController(access, status, resend, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	authorizationHTTP, err := controllers.NewDeviceAuthorizationController(authorizationTestStart(t, f.start), authorizationTestList(t, f), authorizationTestDecide(t, f.decide))
	if err != nil {
		t.Fatal(err)
	}
	proofHTTP, err := controllers.NewDeviceProofController(proofTestStart(t, f.proofStart), proofTestComplete(t, f.proofComplete))
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := personalMiddleware.NewPersonal(f.d.Tokens, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	const base = "/api/mancuerna/v1"
	router.POST(base+"/auth/start", startHTTP.Start)
	router.POST(base+"/auth/check", accessHTTP.Check)
	router.GET(base+"/auth/attempts/:id", accessHTTP.Get)
	router.POST(base+"/auth/device-authorizations", authorizationHTTP.Start)
	router.GET(base+"/me/device-authorizations", middleware, authorizationHTTP.List)
	router.POST(base+"/me/device-authorizations/:id/decision", middleware, authorizationHTTP.Decide)
	router.POST(base+"/me/proofs", middleware, proofHTTP.Start)
	router.POST(base+"/me/proofs/:id/complete", middleware, proofHTTP.Complete)
	call := func(method, path, token, op, secret, proof string, body any) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, base+path, bytes.NewReader(raw))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if op != "" {
			req.Header.Set("Idempotency-Key", op)
		}
		if secret != "" {
			req.Header.Set("X-Attempt-Secret", secret)
		}
		if proof != "" {
			req.Header.Set("X-Device-Proof", proof)
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("sensitive response can be cached")
		}
		var decoded map[string]any
		if json.Unmarshal(res.Body.Bytes(), &decoded) != nil {
			t.Fatal("response is not JSON")
		}
		if res.Code != 200 && (decoded["session"] != nil || decoded["access_token"] != nil) {
			t.Fatal("error response leaked session")
		}
		return res.Code, decoded
	}
	newCandidate := func() (id, secret, candidateID string) {
		t.Helper()
		id, secret, candidateID = uuid.NewString(), base64.RawURLEncoding.EncodeToString([]byte(uuid.NewString())), uuid.NewString()
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		body := map[string]any{"attempt_id": id, "phone": f.phone, "country": "MX", "channel": "sms", "purpose": "access", "candidate": map[string]any{"id": candidateID, "platform": "android", "label": "Synthetic HTTP candidate", "public_key": base64.RawURLEncoding.EncodeToString(pub)}, "new_account_id": uuid.NewString(), "continuation_secret": secret}
		if code, _ := call("POST", "/auth/start", "", uuid.NewString(), "", "", body); code != 200 {
			t.Fatal("candidate HTTP start failed", code)
		}
		code, response := call("POST", "/auth/check", "", uuid.NewString(), secret, "", map[string]any{"attempt_id": id, "code": "123456"})
		if code != 200 || response["status"] != "authorization_required" || response["session"] != nil {
			t.Fatal("OTP alone authorized existing account", code)
		}
		return
	}
	freshProof := func(token string, actor security.AccessClaims, key ed25519.PrivateKey, path, op, decision, purpose string) string {
		t.Helper()
		canonical, err := json.Marshal(map[string]any{"method": "POST", "path": path, "operation_id": op, "body": map[string]any{"decision": decision}})
		if err != nil {
			t.Fatal(err)
		}
		target := sha256.Sum256(canonical)
		id := uuid.NewString()
		code, p := call("POST", "/me/proofs", token, uuid.NewString(), "", "", map[string]any{"id": id, "purpose": purpose, "request_hash": hex.EncodeToString(target[:])})
		if code != 200 || len(p) != 5 || p["request_hash"] != hex.EncodeToString(target[:]) {
			t.Fatal("public proof challenge invalid", code)
		}
		payload, err := json.Marshal(map[string]any{"v": 1, "purpose": p["purpose"], "account_id": actor.AccountID.String(), "session_id": actor.SessionID.String(), "device_id": actor.DeviceID.String(), "proof_id": id, "nonce": p["nonce"], "request_hash": p["request_hash"], "expires_at": p["expires_at"]})
		if err != nil {
			t.Fatal(err)
		}
		signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
		code, completed := call("POST", "/me/proofs/"+id+"/complete", token, uuid.NewString(), "", "", map[string]any{"signature": signature})
		if code != 200 || len(completed) != 2 || completed["expires_at"] != p["expires_at"] {
			t.Fatal("independent public signature rejected", code)
		}
		proof, ok := completed["proof_token"].(string)
		if !ok || len(proof) != 43 {
			t.Fatal("invalid proof token")
		}
		return proof
	}
	attemptID, secret, candidateID := newCandidate()
	authorizationID, requestOp := uuid.NewString(), uuid.NewString()
	requestBody := map[string]any{"attempt_id": attemptID, "id": authorizationID}
	code, requested := call("POST", "/auth/device-authorizations", "", requestOp, secret, "", requestBody)
	if code != 200 || len(requested) != 6 || requested["id"] != authorizationID || requested["status"] != "pending" {
		t.Fatal("authorization request contract invalid", code)
	}
	createdText, ok := requested["created_at"].(string)
	if !ok {
		t.Fatal("new authorization lacks creation time")
	}
	createdAt, createdErr := time.Parse(time.RFC3339Nano, createdText)
	expiresText, ok := requested["expires_at"].(string)
	if !ok {
		t.Fatal("new authorization lacks expiry")
	}
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, expiresText)
	if createdErr != nil || expiresErr != nil || createdAt.IsZero() || !createdAt.Before(expiresAt) {
		t.Fatal("invalid authorization chronology")
	}
	candidate, ok := requested["candidate"].(map[string]any)
	if !ok || len(candidate) != 4 || candidate["id"] != candidateID {
		t.Fatal("candidate snapshot changed")
	}
	if replayCode, replayed := call("POST", "/auth/device-authorizations", "", requestOp, secret, "", requestBody); replayCode != 200 || !reflect.DeepEqual(replayed, requested) {
		t.Fatal("request replay changed comparison data", replayCode)
	}
	if bad, _ := call("POST", "/auth/device-authorizations", "", requestOp, strings.Repeat("x", 43), "", requestBody); bad != 404 {
		t.Fatal("wrong continuation accessed request", bad)
	}
	if code, status := call("GET", "/auth/attempts/"+attemptID, "", "", secret, "", nil); code != 200 || status["status"] != "pending" || status["attempt"].(map[string]any)["state"] != "authorization_pending" || status["session"] != nil {
		t.Fatal("pending authorization status incorrect", code)
	}
	if code, list := call("GET", "/me/device-authorizations?limit=1", bearer, "", "", "", nil); code != 200 || len(list) != 2 || len(list["items"].([]any)) != 1 || !reflect.DeepEqual(list["items"].([]any)[0], requested) {
		t.Fatal("active approver cannot compare exact request", code)
	}
	other := authorizationTestFixture(t)
	otherBearer, _, err := f.d.Tokens.Issue(other.actor.AccountID, other.actor.SessionID, other.actor.DeviceID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code, list := call("GET", "/me/device-authorizations", otherBearer, "", "", "", nil); code != 200 || len(list["items"].([]any)) != 0 {
		t.Fatal("another account listed candidate", code)
	}
	path, decisionOp := "/me/device-authorizations/"+authorizationID+"/decision", uuid.NewString()
	approveBody := map[string]any{"decision": "approve"}
	if code, _ := call("POST", path, bearer, decisionOp, "", "", approveBody); code != 403 {
		t.Fatal("approval accepted without proof", code)
	}
	wrongPurpose := freshProof(bearer, f.actor, f.key, path, decisionOp, "approve", "recovery_key")
	if code, _ := call("POST", path, bearer, decisionOp, "", wrongPurpose, approveBody); code != 403 {
		t.Fatal("wrong purpose approved device", code)
	}
	foreignProof := freshProof(otherBearer, other.actor, other.key, path, decisionOp, "approve", "authorize_device")
	if code, _ := call("POST", path, otherBearer, decisionOp, "", foreignProof, approveBody); code != 404 {
		t.Fatal("account B approved request A", code)
	}
	proof := freshProof(bearer, f.actor, f.key, path, decisionOp, "approve", "authorize_device")
	if code, _ := call("POST", path, bearer, uuid.NewString(), "", proof, approveBody); code != 403 {
		t.Fatal("proof authorized changed operation", code)
	}
	if code, _ := call("POST", path, bearer, decisionOp, "", proof, map[string]any{"decision": "deny"}); code != 403 {
		t.Fatal("proof authorized changed decision", code)
	}
	code, ack := call("POST", path, bearer, decisionOp, "", proof, approveBody)
	if code != 200 || len(ack) != 3 || ack["status"] != "completed" || ack["operation_id"] != decisionOp || ack["completed_at"] == nil || ack["session"] != nil {
		t.Fatal("approver response exposed candidate credentials or invalid Ack", code)
	}
	if code, replay := call("POST", path, bearer, decisionOp, "", proof, approveBody); code != 200 || !reflect.DeepEqual(replay, ack) {
		t.Fatal("lost decision response not replayed", code)
	}
	code, authorized := call("GET", "/auth/attempts/"+attemptID, "", "", secret, "", nil)
	if code != 200 || len(authorized) != 3 || authorized["status"] != "authorized" || authorized["recovery_kit"] != nil {
		t.Fatal("candidate GET did not recover committed session", code)
	}
	sessionJSON, ok := authorized["session"].(map[string]any)
	if !ok || sessionJSON["account_id"] != f.actor.AccountID.String() || sessionJSON["device_id"] != candidateID {
		t.Fatal("candidate recovered wrong actor session")
	}
	candidateClaims, err := f.d.Tokens.Verify(sessionJSON["access_token"].(string), time.Now())
	if err != nil || candidateClaims.DeviceID.String() != candidateID {
		t.Fatal("candidate access JWT invalid")
	}
	for i := 0; i < 2; i++ {
		if code, replayed := call("GET", "/auth/attempts/"+attemptID, "", "", secret, "", nil); code != 200 || !reflect.DeepEqual(replayed, authorized) {
			t.Fatal("candidate GET duplicated result", code)
		}
	}
	if code, list := call("GET", "/me/device-authorizations", bearer, "", "", "", nil); code != 200 || len(list["items"].([]any)) != 0 {
		t.Fatal("approved request remained pending", code)
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.sessions WHERE account_id=?", f.actor.AccountID) != 2 {
		t.Fatal("GET recreated candidate session")
	}
	deniedAttempt, deniedSecret, _ := newCandidate()
	deniedID := uuid.NewString()
	if code, _ := call("POST", "/auth/device-authorizations", "", uuid.NewString(), deniedSecret, "", map[string]any{"attempt_id": deniedAttempt, "id": deniedID}); code != 200 {
		t.Fatal("denial request failed", code)
	}
	denyPath, denyOp := "/me/device-authorizations/"+deniedID+"/decision", uuid.NewString()
	denyProof := freshProof(bearer, f.actor, f.key, denyPath, denyOp, "deny", "authorize_device")
	if code, ack := call("POST", denyPath, bearer, denyOp, "", denyProof, map[string]any{"decision": "deny"}); code != 200 || len(ack) != 3 || ack["session"] != nil {
		t.Fatal("denial failed", code)
	}
	if code, denied := call("GET", "/auth/attempts/"+deniedAttempt, "", "", deniedSecret, "", nil); code != 200 || denied["attempt"].(map[string]any)["state"] != "denied" || denied["session"] != nil {
		t.Fatal("denied candidate obtained authority", code)
	}
	if err = f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, f.actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := call("POST", path, bearer, decisionOp, "", proof, approveBody); code != 401 {
		t.Fatal("revoked approver recovered receipt", code)
	}
	if code, _ := call("GET", "/me/device-authorizations", bearer, "", "", "", nil); code != 401 {
		t.Fatal("revoked approver listed requests", code)
	}
	if code, replayed := call("GET", "/auth/attempts/"+attemptID, "", "", secret, "", nil); code != 200 || !reflect.DeepEqual(replayed, authorized) {
		t.Fatal("approver logout invalidated already committed approval", code)
	}
	if err = f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, candidateClaims.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := call("GET", "/auth/attempts/"+attemptID, "", "", secret, "", nil); code != 401 {
		t.Fatal("revoked candidate recovered session", code)
	}
	if provider.starts.Load() != 2 || provider.checks.Load() != 2 || provider.fetches.Load() != 0 {
		t.Fatal("GET or authorization unexpectedly contacted provider")
	}
}
