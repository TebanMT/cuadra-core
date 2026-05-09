//go:build server

package controllers_test

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/cuadra/cuadra-core/src/modules/notifications/interfaces/controllers"
)

// fakeProcess implements just enough of notiApp.ProcessWebhook to verify
// that "valid signature" reaches the use case and "invalid signature" does
// not. We can't easily inject a fake into the controller without changing
// signatures, so we use a tiny wrapper.
//
// Instead the test exercises the controller end-to-end with a real
// twilio.RequestValidator and a no-op ProcessWebhook (passing nil into the
// constructor's first arg would crash the use case path; we use a stub).
//
// Implementation note: the controller's only branch on signature happens
// before it calls Process.Execute, so we can verify the signature path by
// checking the HTTP status alone — Process.Execute is reached only when the
// signature is valid.

const (
	testAuthToken  = "AUTH_TOKEN_FOR_TEST"
	testWebhookURL = "https://api.entinta.app/api/v1/webhooks/twilio"
)

// computeTwilioSignature mirrors RequestValidator.getValidationSignature for
// urlencoded bodies: HMAC-SHA1 of (URL + sorted "k1v1k2v2..." concat) keyed
// by the auth token, base64 encoded.
func computeTwilioSignature(authToken, requestURL string, params url.Values) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	concat := requestURL
	for _, k := range keys {
		concat += k + params.Get(k)
	}
	h := hmac.New(sha1.New, []byte(authToken))
	_, _ = h.Write([]byte(concat))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// TestWebhookSignature_RejectsInvalidSig confirms that requests without a
// valid X-Twilio-Signature get 401 and never reach the use case.
func TestWebhookSignature_RejectsInvalidSig(t *testing.T) {
	r := newTestRouter(t)
	form := url.Values{}
	form.Set("MessageSid", "SM123")
	form.Set("MessageStatus", "delivered")

	req := httptest.NewRequest(http.MethodPost, testWebhookURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", "obviously-wrong")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestWebhookSignature_AcceptsValidSig(t *testing.T) {
	r := newTestRouter(t)
	form := url.Values{}
	form.Set("MessageSid", "SM123")
	form.Set("MessageStatus", "delivered")

	signed := computeTwilioSignature(testAuthToken, testWebhookURL, form)

	req := httptest.NewRequest(http.MethodPost, testWebhookURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", signed)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// We expect 200 because the (nil) Process.Execute will short-circuit at
	// the use case constructor — we passed a valid Process built around a
	// nil dependency that's not exercised when the row look-up returns
	// (nil, nil). See newTestRouter.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestWebhookSignature_MissingHeaderRejected(t *testing.T) {
	r := newTestRouter(t)
	form := url.Values{}
	form.Set("MessageSid", "SM123")
	form.Set("MessageStatus", "delivered")

	req := httptest.NewRequest(http.MethodPost, testWebhookURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// newTestRouter builds a controller wired with a noop ProcessWebhook (the
// dependency is satisfied via a fake repo and uow that immediately return
// (nil, nil) for the look-up paths exercised by the test).
func newTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	process := newNoopProcess()
	ctrl := controllers.NewWebhookController(process, testAuthToken, testWebhookURL)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	ctrl.RegisterRoutes(r)
	return r
}
