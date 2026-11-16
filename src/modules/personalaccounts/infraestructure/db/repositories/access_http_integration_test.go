//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	hashing "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/budget"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// HTTP parsing -> application commands -> real restricted PostgreSQL -> JSON.
// Only Verify is simulated. This does not certify delivery of a real SMS.
func TestAccessHTTPPostgresSignupStatusReplayAndExistingAccount(t *testing.T) {
	d := finalizationDependencies(t)
	provider := &phoneProviderDouble{}
	now := time.Now().UTC().Truncate(time.Millisecond)
	provider.start = func(_ context.Context, in verification.StartRequest) (verification.Result, error) {
		return verification.Result{Reference: "VE-http-synthetic", Channel: in.Channel, State: verification.Pending, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}, nil
	}
	provider.check = func(_ context.Context, in verification.CheckRequest) (verification.Result, error) {
		state := verification.Pending
		if in.Code == "123456" {
			state = verification.Approved
		}
		return verification.Result{Reference: in.Reference, Channel: verification.SMS, State: state, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}, nil
	}
	rules, err := budget.NewRules(map[budget.Scope]budget.Limit{budget.Destination: {Window: time.Hour, MaxSend: 10, MaxCheck: 20}, budget.Origin: {Window: time.Hour, MaxSend: 10, MaxCheck: 20}, budget.Service: {Window: time.Hour, MaxSend: 10, MaxCheck: 20}})
	if err != nil {
		t.Fatal(err)
	}
	budgetKey := make([]byte, 32)
	if _, err = rand.Read(budgetKey); err != nil {
		t.Fatal(err)
	}
	subjects, err := hashing.NewSubjects(budgetKey)
	if err != nil {
		t.Fatal(err)
	}
	phone := personalApp.StartAuthDependencies{UoW: d.UoW, Attempts: d.Attempts, Receipts: d.Receipts, Budgets: NewOTPBudgetRepository(), Rules: rules, Subjects: subjects, Provider: provider, ServiceIdentity: uuid.NewString(), ProviderValidity: 10 * time.Minute, Now: time.Now}
	start, err := personalApp.NewStartAuth(phone)
	if err != nil {
		t.Fatal(err)
	}
	check, err := personalApp.NewCheckPhoneVerification(phone, 5)
	if err != nil {
		t.Fatal(err)
	}
	finalize, err := personalApp.NewFinalizeAccess(d)
	if err != nil {
		t.Fatal(err)
	}
	access, err := personalApp.NewCheckAccess(check, finalize)
	if err != nil {
		t.Fatal(err)
	}
	get, err := personalApp.NewGetAttempt(personalApp.GetAttemptDependencies{UoW: d.UoW, Attempts: d.Attempts, Provider: provider, Now: time.Now, ProviderTimeout: 20 * time.Second, ReconcileAfter: 25 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authorizationStatus, err := personalApp.NewGetDeviceAuthorizationStatus(personalApp.GetDeviceAuthorizationStatusDependencies{UoW: d.UoW, Attempts: d.Attempts, Authorizations: NewDeviceAuthorizationRepository(), Sessions: d.Sessions, Receipts: d.Receipts, Envelopes: d.Envelopes, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	recoveryStatus, err := personalApp.NewGetRecoveryStatus(personalApp.GetRecoveryStatusDependencies{UoW: d.UoW, Attempts: d.Attempts, Recovery: d.Recovery, Sessions: d.Sessions, Receipts: d.Receipts, RecoverySecrets: d.RecoverySecrets, Envelopes: d.Envelopes, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	status, err := personalApp.NewGetAuthStatus(get, finalize, authorizationStatus, recoveryStatus)
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
	gin.SetMode(gin.TestMode)
	router := gin.New()
	base := "/api/mancuerna/v1"
	router.POST(base+"/auth/start", startHTTP.Start)
	router.POST(base+"/auth/check", accessHTTP.Check)
	router.GET(base+"/auth/attempts/:id", accessHTTP.Get)
	router.POST(base+"/auth/attempts/:id/resend", accessHTTP.Resend)
	getMe, err := personalApp.NewGetMe(personalApp.GetMeDependencies{UoW: d.UoW, Sessions: d.Sessions, Me: NewMeRepository(), Capabilities: me.Capabilities{ProtocolVersion: 1, MinimumProtocolVersion: 1, MinimumAppVersion: "0.1.0", MaxPushOperations: 50, MaxRequestBytes: 2097152, MaxGroupBytes: 1048576, Features: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	confirm, err := personalApp.NewConfirmRecoveryKey(personalApp.ConfirmRecoveryKeyDependencies{UoW: d.UoW, Sessions: d.Sessions, Recovery: d.Recovery, Receipts: d.Receipts, Events: d.Events, Envelopes: d.Envelopes, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	meHTTP, err := controllers.NewMeController(getMe, confirm)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := personalMiddleware.NewPersonal(d.Tokens, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	router.GET(base+"/me", middleware, meHTTP.Get)
	router.POST(base+"/me/recovery-key/confirm", middleware, meHTTP.ConfirmRecovery)
	authPool := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	resolver, err := NewRefreshIdentityResolver(authPool)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := personalApp.NewRefreshSession(personalApp.RefreshSessionDependencies{UoW: d.UoW, Identity: resolver, Rotations: NewRefreshSessionRepository(), Sessions: d.Sessions, Events: d.Events, Tokens: d.Tokens, Proofs: d.Proofs, Envelopes: d.Envelopes, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	logoutRepo, err := NewLogoutRepository(authPool)
	if err != nil {
		t.Fatal(err)
	}
	logout, err := personalApp.NewLogout(personalApp.LogoutDependencies{UoW: d.UoW, Logout: logoutRepo, Sessions: d.Sessions, Receipts: d.Receipts, Events: d.Events, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	sessionHTTP, err := controllers.NewSessionController(refresh, logout)
	if err != nil {
		t.Fatal(err)
	}
	router.POST(base+"/auth/refresh", sessionHTTP.Refresh)
	router.POST(base+"/auth/logout", sessionHTTP.Logout)

	profileHTTP, err := controllers.NewProfileCommandController(profileCommandUseCase(t, profileCommandDependencies(t)))
	if err != nil {
		t.Fatal(err)
	}
	router.POST(base+"/commands", middleware, profileHTTP.Execute)
	privateByDevice := map[string]ed25519.PrivateKey{}
	bearer := ""
	call := func(method, path, operation, secret string, body any) (int, map[string]any) {
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
		if operation != "" {
			req.Header.Set("Idempotency-Key", operation)
		}
		if secret != "" {
			req.Header.Set("X-Attempt-Secret", secret)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("response cacheable")
		}
		var response map[string]any
		if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal("invalid response JSON")
		}
		if strings.Contains(w.Body.String(), "provider_reference") || strings.Contains(w.Body.String(), "continuation_hash") || strings.Contains(w.Body.String(), "request_hash") {
			t.Fatal("private field exposed")
		}
		return w.Code, response
	}
	startBody := func() map[string]any {
		t.Helper()
		pub, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		deviceID := uuid.NewString()
		privateByDevice[deviceID] = private
		return map[string]any{"attempt_id": uuid.NewString(), "new_account_id": uuid.NewString(), "continuation_secret": strings.Repeat("x", 43), "phone": "+525555555710", "country": "MX", "channel": "sms", "purpose": "access", "candidate": map[string]any{"id": deviceID, "platform": "android", "label": "Synthetic HTTP phone", "public_key": base64.RawURLEncoding.EncodeToString(pub)}}
	}
	body := startBody()
	attemptID := body["attempt_id"].(string)
	secret := body["continuation_secret"].(string)
	startOp := uuid.NewString()
	if code, res := call("POST", "/auth/start", startOp, "", body); code != 200 || res["state"] != "pending" {
		t.Fatal("start failed", code, res["code"])
	}
	if code, _ := call("POST", "/auth/start", startOp, "", body); code != 200 {
		t.Fatal("start replay failed", code)
	}
	checkBody := map[string]any{"attempt_id": attemptID, "code": "000000"}
	if code, res := call("POST", "/auth/check", uuid.NewString(), secret, checkBody); code != 200 || res["status"] != "pending" {
		t.Fatal("wrong OTP authorized", code, res["status"])
	}
	checkBody["code"] = "123456"
	checkOp := uuid.NewString()
	code, first := call("POST", "/auth/check", checkOp, secret, checkBody)
	if code != 200 || first["status"] != "authorized" || first["recovery_kit"] == nil {
		t.Fatal("signup failed", code, first["code"])
	}
	bearer = first["session"].(map[string]any)["access_token"].(string)
	if code, view := call("GET", "/me", "", "", nil); code != 200 || view["recovery_prepared"] != false || view["phone_masked"] != "•••• 5710" {
		t.Fatal("initial account view incorrect", code, view["code"])
	}
	confirmBody := map[string]any{"recovery_key_id": first["recovery_kit"].(map[string]any)["id"], "saved": true}
	confirmOp := uuid.NewString()
	code, ack := call("POST", "/me/recovery-key/confirm", confirmOp, "", confirmBody)
	if code != 200 || ack["status"] != "completed" {
		t.Fatal("kit confirmation failed", code, ack["code"])
	}
	if code, repeated := call("POST", "/me/recovery-key/confirm", confirmOp, "", confirmBody); code != 200 || !reflect.DeepEqual(ack, repeated) {
		t.Fatal("confirmation retry changed result")
	}
	if code, view := call("GET", "/me", "", "", nil); code != 200 || view["recovery_prepared"] != true {
		t.Fatal("saved kit not reflected", code)
	}

	accountSession := first["session"].(map[string]any)
	profileOp := uuid.NewString()
	profileBody := map[string]any{"operation_id": profileOp, "origin_device_id": accountSession["device_id"], "type": "profile.update", "entity_id": accountSession["account_id"], "base": map[string]any{"revision": "1"}, "captured_at": now.Format("2006-01-02T15:04:05.000Z"), "payload": map[string]any{"display_name": "Perfil HTTP", "weight_unit": "lb"}}
	if code, _ := call("POST", "/commands", uuid.NewString(), "", profileBody); code != 400 {
		t.Fatal("different operation header accepted", code)
	}

	profileBody["base"] = map[string]any{"revision": "1", "after_operation_id": nil}
	if code, _ := call("POST", "/commands", profileOp, "", profileBody); code != 400 {
		t.Fatal("null alternative changed command before hashing", code)
	}
	profileBody["base"] = map[string]any{"revision": "1"}
	code, profileResult := call("POST", "/commands", profileOp, "", profileBody)
	if code != 200 || profileResult["outcome"] != "accepted" {
		t.Fatal("profile command failed", code, profileResult["code"])
	}
	if code, retry := call("POST", "/commands", profileOp, "", profileBody); code != 200 || !reflect.DeepEqual(profileResult, retry) {
		t.Fatal("profile retry diverged", code)
	}
	if code, view := call("GET", "/me", "", "", nil); code != 200 || view["profile"].(map[string]any)["display_name"] != "Perfil HTTP" || view["profile"].(map[string]any)["weight_unit"] != "lb" || view["profile"].(map[string]any)["revision"] != "2" {
		t.Fatal("Me did not show accepted profile", code)
	}
	staleOp := uuid.NewString()
	profileBody["operation_id"] = staleOp
	code, conflict := call("POST", "/commands", staleOp, "", profileBody)
	if code != 200 || conflict["outcome"] != "conflict" || conflict["conflict_id"] == nil {
		t.Fatal("stale profile proposal lost", code)
	}
	code, replayed := call("POST", "/auth/check", checkOp, secret, checkBody)
	if code != 200 || !reflect.DeepEqual(first, replayed) {
		t.Fatal("retry changed original credentials")
	}
	code, replayed = call("GET", "/auth/attempts/"+attemptID, "", secret, nil)
	if code != 200 || !reflect.DeepEqual(first, replayed) {
		t.Fatal("status lost authorized result")
	}
	if code, _ := call("GET", "/auth/attempts/"+attemptID, "", strings.Repeat("z", 43), nil); code != 404 {
		t.Fatal("wrong secret revealed attempt", code)
	}
	changed := map[string]any{"attempt_id": attemptID, "code": "654321"}
	if code, res := call("POST", "/auth/check", checkOp, secret, changed); code != 409 || res["code"] != "IDEMPOTENCY_MISMATCH" {
		t.Fatal("altered retry accepted", code)
	}
	second := startBody()
	if code, _ := call("POST", "/auth/start", uuid.NewString(), "", second); code != 200 {
		t.Fatal("second attempt failed")
	}
	code, result := call("POST", "/auth/check", uuid.NewString(), secret, map[string]any{"attempt_id": second["attempt_id"], "code": "123456"})
	if code != 200 || result["status"] != "authorization_required" || result["session"] != nil || result["recovery_kit"] != nil {
		t.Fatal("existing account disclosed authority", code)
	}
	if provider.starts.Load() != 2 || provider.checks.Load() != 3 || provider.fetches.Load() != 0 {
		t.Fatal("HTTP retry repeated external effects")
	}
	session := first["session"].(map[string]any)
	sessionID := uuid.MustParse(session["session_id"].(string))
	refreshToken := session["refresh_token"].(string)
	refreshOp := uuid.New()
	payload, err := crypto.RefreshProofPayload(security.RefreshProof{OperationID: refreshOp, SessionID: sessionID, Generation: 1, RefreshHash: sha256.Sum256([]byte(refreshToken))})
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateByDevice[session["device_id"].(string)], payload))
	refreshBody := map[string]any{"refresh_token": refreshToken, "device_signature": signature}
	code, rotated := call("POST", "/auth/refresh", refreshOp.String(), "", refreshBody)
	if code != 200 || rotated["generation"] != "2" || rotated["session_id"] != session["session_id"] {
		t.Fatal("refresh changed identity or failed", code, rotated["code"])
	}
	if code, replayed := call("POST", "/auth/refresh", refreshOp.String(), "", refreshBody); code != 200 || !reflect.DeepEqual(rotated, replayed) {
		t.Fatal("refresh response loss changed credentials")
	}
	bearer = rotated["access_token"].(string)
	logoutOp := uuid.NewString()
	logoutBody := map[string]any{"revocation_ticket": session["revocation_ticket"]}
	code, logoutAck := call("POST", "/auth/logout", logoutOp, "", logoutBody)
	if code != 200 || logoutAck["status"] != "completed" {
		t.Fatal("old ticket failed to revoke after rotation", code, logoutAck["code"])
	}
	if code, replayed := call("POST", "/auth/logout", logoutOp, "", logoutBody); code != 200 || !reflect.DeepEqual(logoutAck, replayed) {
		t.Fatal("logout retry changed acknowledgement")
	}
	if code, res := call("GET", "/me", "", "", nil); code != 401 || res["code"] != "SESSION_REVOKED" {
		t.Fatal("new access JWT ignored logout", code, res["code"])
	}
	if code, res := call("GET", "/auth/attempts/"+attemptID, "", secret, nil); code != 401 || res["code"] != "SESSION_REVOKED" {
		t.Fatal("revoked session recovered", code, res["code"])
	}
}
