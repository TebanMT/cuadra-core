//go:build server

package controllers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	"github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/whatsapp"
	"github.com/cuadra/cuadra-core/src/modules/notifications/interfaces/controllers"
	"github.com/cuadra/cuadra-core/src/shared/auth"
)

// TestWhatsappStart_SendsOTPViaProvider verifies that POST
// /gyms/me/whatsapp/start invokes provider.SendOTP with the same code that
// the OTP store issued, before returning 200. This is the contract the
// production Twilio path relies on (UC-037 connect-step).
func TestWhatsappStart_SendsOTPViaProvider(t *testing.T) {
	provider := whatsapp.NewMockProvider()
	r, accessToken := newWhatsappRouter(t, provider)

	body, _ := json.Marshal(map[string]string{"phone_number": "+524421112233"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gyms/me/whatsapp/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if provider.SentCount() != 1 {
		t.Fatalf("provider.SentCount() = %d, want 1", provider.SentCount())
	}
	last := provider.LastSent()
	if last == nil || !last.IsOTP {
		t.Fatalf("expected an OTP send, got %+v", last)
	}
	if last.Recipient != "+524421112233" {
		t.Errorf("recipient = %s", last.Recipient)
	}
	if len(last.OTPCode) != 6 {
		t.Errorf("expected 6-digit code, got %q", last.OTPCode)
	}
	// In dev (WHATSAPP_PROVIDER unset/mock) the response also surfaces the
	// code so the operator can complete the flow without WhatsApp; assert
	// the surfaced code matches what the provider received.
	var resp struct {
		DevCode string `json:"dev_code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.DevCode != "" && resp.DevCode != last.OTPCode {
		t.Errorf("dev_code (%q) != provider.OTPCode (%q)", resp.DevCode, last.OTPCode)
	}
}

// TestWhatsappStart_FailedSendReturns502 asserts the controller surfaces a
// provider error as 502 instead of silently swallowing it. The OTP store
// retains the entry so the operator can re-trigger /start without losing
// the issued code.
func TestWhatsappStart_FailedSendReturns502(t *testing.T) {
	provider := whatsapp.NewMockProvider()
	provider.FailNextHard = true
	r, accessToken := newWhatsappRouter(t, provider)

	body, _ := json.Marshal(map[string]string{"phone_number": "+524421112233"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gyms/me/whatsapp/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if provider.SentCount() != 0 {
		t.Errorf("provider.SentCount() = %d on hard failure, want 0", provider.SentCount())
	}
}

// newWhatsappRouter wires a minimal Controller with the provided mock
// provider and a one-off owner JWT. The use cases passed to NewController
// are nil — handleWhatsappStart never reaches them, and the test routes
// only POST /start.
func newWhatsappRouter(t *testing.T, provider *whatsapp.MockProvider) (*gin.Engine, string) {
	t.Helper()
	tokens := auth.NewJWTService("test-secret")
	gin.SetMode(gin.TestMode)

	ctrl := controllers.NewController(
		(*notiApp.ConnectWhatsApp)(nil),
		(*notiApp.DisconnectWhatsApp)(nil),
		(*notiApp.GetWhatsAppStatus)(nil),
		(*notiApp.ListTemplates)(nil),
		(*notiApp.UpdateTemplate)(nil),
		(*notiApp.Broadcast)(nil),
		(*notiApp.ListNotifications)(nil),
		(*notiApp.RetryNotification)(nil),
		(*notiApp.ListOwnerAlerts)(nil),
		(*notiApp.UpdateOwnerAlert)(nil),
		provider,
		tokens,
	)

	r := gin.New()
	ctrl.RegisterRoutes(r)

	access, err := tokens.GenerateAccessToken(uuid.New(), uuid.New(), "owner")
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return r, access
}
