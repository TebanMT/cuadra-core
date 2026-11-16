//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/me"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Every account effect, JWT, receipt and HTTP controller is real. OTP delivery
// alone is a deterministic double; this is not a real Verify/mobile test.
func TestRecoveryHTTPPostgresVerifiedPhoneLostResponseAndRetiredSessions(t *testing.T) {
	f := completeRecoveryTestFixture(t, 5)
	provider := &phoneProviderDouble{}
	providerAt := time.Now().UTC().Truncate(time.Millisecond)
	provider.start = func(_ context.Context, in verification.StartRequest) (verification.Result, error) {
		return verification.Result{Reference: "VE-recovery-http-synthetic", Channel: in.Channel, State: verification.Pending, CreatedAt: providerAt, ExpiresAt: providerAt.Add(10 * time.Minute)}, nil
	}
	provider.check = func(_ context.Context, in verification.CheckRequest) (verification.Result, error) {
		return verification.Result{Reference: in.Reference, Channel: verification.SMS, State: verification.Approved, CreatedAt: providerAt, ExpiresAt: providerAt.Add(10 * time.Minute)}, nil
	}
	rules, err := budget.NewRules(map[budget.Scope]budget.Limit{budget.Destination: {Window: time.Hour, MaxSend: 5, MaxCheck: 10}, budget.Origin: {Window: time.Hour, MaxSend: 5, MaxCheck: 10}, budget.Service: {Window: time.Hour, MaxSend: 5, MaxCheck: 10}})
	if err != nil {
		t.Fatal(err)
	}
	phone := personalApp.StartAuthDependencies{UoW: f.d.UoW, Attempts: f.d.Attempts, Receipts: f.d.Receipts, Budgets: f.d.Budgets, Rules: rules, Subjects: f.d.Subjects, Provider: provider, ServiceIdentity: uuid.NewString(), ProviderValidity: 10 * time.Minute, Now: time.Now}
	start, err := personalApp.NewStartAuth(phone)
	if err != nil {
		t.Fatal(err)
	}
	check, err := personalApp.NewCheckPhoneVerification(phone, 5)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewIdentityAccessRepository(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	finalize, err := personalApp.NewFinalizeAccess(personalApp.FinalizeAccessDependencies{UoW: f.d.UoW, Attempts: f.d.Attempts, Identity: identity, Sessions: f.d.Sessions, Recovery: f.d.Recovery, Receipts: f.d.Receipts, Events: f.d.Events, Tokens: f.d.Tokens, Proofs: crypto.DeviceProofs{}, RecoverySecrets: f.d.RecoverySecrets, Envelopes: f.d.Envelopes, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	access, err := personalApp.NewCheckAccess(check, finalize)
	if err != nil {
		t.Fatal(err)
	}
	get, err := personalApp.NewGetAttempt(personalApp.GetAttemptDependencies{UoW: f.d.UoW, Attempts: f.d.Attempts, Provider: provider, Now: time.Now, ProviderTimeout: 20 * time.Second, ReconcileAfter: 25 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authz, err := personalApp.NewGetDeviceAuthorizationStatus(personalApp.GetDeviceAuthorizationStatusDependencies{UoW: f.d.UoW, Attempts: f.d.Attempts, Authorizations: NewDeviceAuthorizationRepository(), Sessions: f.d.Sessions, Receipts: f.d.Receipts, Envelopes: f.d.Envelopes, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	resultNow := time.Now
	read := f.read
	read.Now = func() time.Time { return resultNow() }
	status, err := personalApp.NewGetAuthStatus(get, finalize, authz, completeRecoveryTestReader(t, read))
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
	recoveryHTTP, err := controllers.NewRecoveryController(completeRecoveryTestUseCase(t, f.d))
	if err != nil {
		t.Fatal(err)
	}
	getMe, err := personalApp.NewGetMe(personalApp.GetMeDependencies{UoW: f.d.UoW, Sessions: f.d.Sessions, Me: NewMeRepository(), Capabilities: me.Capabilities{ProtocolVersion: 1, MinimumProtocolVersion: 1, MinimumAppVersion: "0.1.0", MaxPushOperations: 50, MaxRequestBytes: 2097152, MaxGroupBytes: 1048576, Features: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	confirm, err := personalApp.NewConfirmRecoveryKey(personalApp.ConfirmRecoveryKeyDependencies{UoW: f.d.UoW, Sessions: f.d.Sessions, Recovery: f.d.Recovery, Receipts: f.d.Receipts, Events: f.d.Events, Envelopes: f.d.Envelopes, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	meHTTP, err := controllers.NewMeController(getMe, confirm)
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
	router.POST(base+"/auth/recovery/complete", recoveryHTTP.Complete)
	router.GET(base+"/me", middleware, meHTTP.Get)
	call := func(method, path, bearer, op, secret string, body any) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, base+path, bytes.NewReader(raw))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if op != "" {
			req.Header.Set("Idempotency-Key", op)
		}
		if secret != "" {
			req.Header.Set("X-Attempt-Secret", secret)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("recovery response cacheable")
		}
		var out map[string]any
		if json.Unmarshal(response.Body.Bytes(), &out) != nil {
			t.Fatal("invalid recovery JSON")
		}
		if response.Code != 200 && (out["session"] != nil || out["recovery_kit"] != nil) {
			t.Fatal("failed recovery exposed credentials")
		}
		return response.Code, out
	}
	attemptID, candidateID, secret := uuid.NewString(), uuid.NewString(), base64.RawURLEncoding.EncodeToString([]byte(uuid.NewString()))
	newPhone := accessTestPhone()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	startBody := map[string]any{"attempt_id": attemptID, "phone": newPhone, "country": "MX", "channel": "sms", "purpose": "recovery", "candidate": map[string]any{"id": candidateID, "platform": "ios", "label": "Synthetic recovered installation", "public_key": base64.RawURLEncoding.EncodeToString(pub)}, "new_account_id": uuid.NewString(), "continuation_secret": secret}
	if code, _ := call("POST", "/auth/start", "", uuid.NewString(), "", startBody); code != 200 {
		t.Fatal("HTTP recovery start failed", code)
	}
	if code, checked := call("POST", "/auth/check", "", uuid.NewString(), secret, map[string]any{"attempt_id": attemptID, "code": "123456"}); code != 200 || checked["status"] != "authorization_required" || checked["session"] != nil {
		t.Fatal("OTP alone granted recovery", code)
	}
	if code, pending := call("GET", "/auth/attempts/"+attemptID, "", "", secret, nil); code != 200 || pending["status"] != "authorization_required" || pending["session"] != nil {
		t.Fatal("GET finalized unconsumed recovery", code)
	}
	body := map[string]any{"attempt_id": attemptID, "recovery_key_id": f.original.RecoveryKit.ID, "recovery_secret": f.original.RecoveryKit.Secret}
	if code, _ := call("POST", "/auth/recovery/complete", "", uuid.NewString(), secret, body); code != 400 {
		t.Fatal("missing confirmation boolean accepted", code)
	}
	body["confirm_phone_change"] = false
	if code, _ := call("POST", "/auth/recovery/complete", "", uuid.NewString(), secret, body); code != 409 {
		t.Fatal("unconfirmed phone replacement accepted", code)
	}
	body["confirm_phone_change"] = true
	body["recovery_secret"] = strings.Repeat("A", 43)
	wrongOp := uuid.NewString()
	if code, _ := call("POST", "/auth/recovery/complete", "", wrongOp, secret, body); code != 409 {
		t.Fatal("wrong kit returned non-generic outcome", code)
	}
	before := completeRecoveryTestBudget(t, f, uuid.MustParse(f.original.RecoveryKit.ID))
	if code, _ := call("POST", "/auth/recovery/complete", "", wrongOp, secret, body); code != 409 || completeRecoveryTestBudget(t, f, uuid.MustParse(f.original.RecoveryKit.ID)) != before {
		t.Fatal("wrong-key retry charged again", code)
	}
	if !recoveryTestGet(t, f.d.UoW, NewRecoveryKeyRepository(), f.actor.AccountID, uuid.MustParse(f.original.RecoveryKit.ID)).Live() {
		t.Fatal("wrong key consumed actual kit")
	}
	if code, _ := call("GET", "/me", f.original.Session.AccessToken, "", "", nil); code != 200 {
		t.Fatal("failed recovery revoked previous session", code)
	}
	body["recovery_secret"] = f.original.RecoveryKit.Secret
	if code, _ := call("POST", "/auth/recovery/complete", "", wrongOp, secret, body); code != 409 {
		t.Fatal("changed operation replaced rejected receipt", code)
	}
	op := uuid.NewString()
	code, authorized := call("POST", "/auth/recovery/complete", "", op, secret, body)
	if code != 200 || len(authorized) != 3 || authorized["status"] != "authorized" {
		t.Fatal("complete recovery contract invalid", code)
	}
	tokens, ok := authorized["session"].(map[string]any)
	if !ok || len(tokens) != 9 || tokens["account_id"] != f.actor.AccountID.String() || tokens["device_id"] != candidateID || tokens["generation"] != "1" {
		t.Fatal("recovery issued credentials to wrong candidate")
	}
	kit, ok := authorized["recovery_kit"].(map[string]any)
	if !ok || len(kit) != 3 || kit["id"] == f.original.RecoveryKit.ID || kit["generation"] != "2" {
		t.Fatal("recovery did not replace kit")
	}
	if code, me := call("GET", "/me", tokens["access_token"].(string), "", "", nil); code != 200 || me["recovery_prepared"] != false || me["security_version"] != "2" {
		t.Fatal("new session/kit state invalid", code)
	}
	if code, _ := call("GET", "/me", f.original.Session.AccessToken, "", "", nil); code != 401 {
		t.Fatal("previous JWT still authorizes data", code)
	}
	if code, replay := call("POST", "/auth/recovery/complete", "", op, secret, body); code != 200 || !reflect.DeepEqual(replay, authorized) {
		t.Fatal("lost completion response changed result", code)
	}
	if code, replay := call("GET", "/auth/attempts/"+attemptID, "", "", secret, nil); code != 200 || !reflect.DeepEqual(replay, authorized) {
		t.Fatal("GET did not recover committed result", code)
	}
	if code, _ := call("GET", "/auth/attempts/"+attemptID, "", "", strings.Repeat("x", 43), nil); code != 404 {
		t.Fatal("foreign continuation recovered credentials", code)
	}
	resultNow = func() time.Time { return time.Now().Add(11 * time.Minute) }
	if code, expired := call("GET", "/auth/attempts/"+attemptID, "", "", secret, nil); code != 410 || expired["code"] != "RESULT_EXPIRED" {
		t.Fatal("expired result window returned credentials", code)
	}
	resultNow = time.Now
	if err = f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, uuid.MustParse(tokens["session_id"].(string)), time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := call("GET", "/auth/attempts/"+attemptID, "", "", secret, nil); code != 401 {
		t.Fatal("revoked recovered session replayed credentials", code)
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.sessions WHERE account_id=?", f.actor.AccountID) != 2 || provider.starts.Load() != 1 || provider.checks.Load() != 1 || provider.fetches.Load() != 0 {
		t.Fatal("recovery replay duplicated session or contacted provider")
	}
}
