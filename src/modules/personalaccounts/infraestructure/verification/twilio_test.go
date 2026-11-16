//go:build server

package verification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	phoneVerification "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
)

const syntheticAccount = "ACaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const syntheticService = "VAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const syntheticReference = "VEcccccccccccccccccccccccccccccccc"
const syntheticPhone = "+525500000099"
const syntheticPassword = "synthetic-password-do-not-log"

func configFixture() TwilioConfig {
	return TwilioConfig{AccountSID: syntheticAccount, ServiceSID: syntheticService, Username: syntheticAccount, Password: syntheticPassword, ServiceValidity: 7 * time.Minute, RequestTimeout: time.Second}
}
func bindingFixture() phoneVerification.Binding {
	return phoneVerification.Binding{Reference: syntheticReference, DestinationE164: syntheticPhone}
}
func resourceFixture(status string) map[string]any {
	return map[string]any{"sid": syntheticReference, "service_sid": syntheticService, "account_sid": syntheticAccount, "to": syntheticPhone, "channel": "sms", "status": status, "date_created": "2026-10-19T03:00:00Z", "valid": false, "send_code_attempts": []map[string]string{{"channel": "sms", "attempt_sid": "VLdddddddddddddddddddddddddddddddd"}}}
}
func fixtureBytes(t *testing.T, resource map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func loopbackProvider(t *testing.T, handler http.HandlerFunc) *Twilio {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	p, err := newTwilio(configFixture(), server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func assertFailure(t *testing.T, err error, kind phoneVerification.FailureKind) *phoneVerification.Failure {
	t.Helper()
	var f *phoneVerification.Failure
	if !errors.As(err, &f) || f.Kind != kind {
		t.Fatalf("got %v; want %s", err, kind)
	}
	for _, secret := range []string{syntheticPhone, syntheticPassword, "123456", "verify.twilio.com", "provider leaked message"} {
		if strings.Contains(fmt.Sprintf("%+v", err), secret) {
			t.Fatal("failure exposed upstream material")
		}
	}
	return f
}

func TestTwilioConfigurationRequiresKnownValidityAndTimeout(t *testing.T) {
	for name, mutate := range map[string]func(*TwilioConfig){
		"missing account":    func(c *TwilioConfig) { c.AccountSID = "" },
		"invalid service":    func(c *TwilioConfig) { c.ServiceSID = "VA/path" },
		"missing username":   func(c *TwilioConfig) { c.Username = "" },
		"another account":    func(c *TwilioConfig) { c.Username = "ACffffffffffffffffffffffffffffffff" },
		"missing password":   func(c *TwilioConfig) { c.Password = " " },
		"unknown validity":   func(c *TwilioConfig) { c.ServiceValidity = 0 },
		"too short validity": func(c *TwilioConfig) { c.ServiceValidity = time.Minute },
		"too long validity":  func(c *TwilioConfig) { c.ServiceValidity = 25 * time.Hour },
		"missing timeout":    func(c *TwilioConfig) { c.RequestTimeout = 0 },
		"unbounded timeout":  func(c *TwilioConfig) { c.RequestTimeout = time.Minute },
	} {
		t.Run(name, func(t *testing.T) {
			c := configFixture()
			mutate(&c)
			_, err := NewTwilio(c)
			assertFailure(t, err, phoneVerification.InvalidConfiguration)
		})
	}
	c := configFixture()
	c.Username = "SKdddddddddddddddddddddddddddddddd"
	if _, err := NewTwilio(c); err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(rendered, syntheticPassword) {
			t.Fatal("configuration exposed credentials")
		}
	}
}

func TestTwilioStartSMSWhatsAppAndExplicitResendPreserveExpiry(t *testing.T) {
	var calls atomic.Int32
	p := loopbackProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v2/Services/"+syntheticService+"/Verifications" || r.URL.RawQuery != "" {
			t.Error("unexpected send request")
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != syntheticAccount || password != syntheticPassword {
			t.Error("incorrect provider authentication")
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("incorrect form encoding")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.PostForm.Get("To") != syntheticPhone || len(r.PostForm) != 2 {
			t.Error("unexpected send fields")
		}
		resource := resourceFixture("pending")
		resource["channel"] = r.PostForm.Get("Channel")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resource)
	})
	var originalExpiry time.Time
	for _, channel := range []phoneVerification.Channel{phoneVerification.WhatsApp, phoneVerification.SMS, phoneVerification.SMS} {
		result, err := p.Start(context.Background(), phoneVerification.StartRequest{DestinationE164: syntheticPhone, Channel: channel})
		if err != nil {
			t.Fatal(err)
		}
		if result.State != phoneVerification.Pending || result.Channel != channel || result.ExpiresAt.Sub(result.CreatedAt) != 7*time.Minute {
			t.Fatal("incorrect send facts")
		}
		if originalExpiry.IsZero() {
			originalExpiry = result.ExpiresAt
		} else if result.ExpiresAt != originalExpiry {
			t.Fatal("resend extended provider expiry")
		}
	}
	if calls.Load() != 3 {
		t.Fatal("provider performed an implicit send")
	}
}

func TestTwilioCheckBindsReferenceAndUsesStatus(t *testing.T) {
	for _, state := range []string{"pending", "approved", "canceled", "expired", "max_attempts_reached", "failed", "deleted"} {
		t.Run(state, func(t *testing.T) {
			p := loopbackProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/Services/"+syntheticService+"/VerificationCheck" || r.Method != "POST" || r.URL.RawQuery != "" {
					t.Error("unexpected check route")
				}
				_ = r.ParseForm()
				if r.PostForm.Get("VerificationSid") != syntheticReference || r.PostForm.Get("Code") != "123456" || len(r.PostForm) != 2 {
					t.Error("check is not bound to stored reference")
				}
				resource := resourceFixture(state)
				resource["valid"] = state != "approved" // deliberately contradict deprecated valid
				_ = json.NewEncoder(w).Encode(resource)
			})
			result, err := p.Check(context.Background(), phoneVerification.CheckRequest{Binding: bindingFixture(), Code: "123456"})
			if err != nil {
				t.Fatal(err)
			}
			want, _ := providerState(state)
			if result.State != want {
				t.Fatal("status did not determine result")
			}
		})
	}
}

func TestTwilioFetchReadsOnlyKnownReference(t *testing.T) {
	var calls atomic.Int32
	p := loopbackProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/v2/Services/"+syntheticService+"/Verifications/"+syntheticReference || r.URL.RawQuery != "" {
			t.Error("unexpected fetch")
		}
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			t.Error("fetch sent body")
		}
		_ = json.NewEncoder(w).Encode(resourceFixture("pending"))
	})
	result, err := p.Fetch(context.Background(), bindingFixture())
	if err != nil || result.State != phoneVerification.Pending {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("fetch repeated")
	}
}

func TestTwilioErrorsNeverTreatMissingAsApprovedOrRetry(t *testing.T) {
	for _, test := range []struct {
		name         string
		status, code int
		kind         phoneVerification.FailureKind
	}{
		{"missing after success expiry or limit", 404, 20404, phoneVerification.Missing},
		{"check attempts", 429, 60202, phoneVerification.CheckLimitReached},
		{"send attempts", 429, 60203, phoneVerification.RateLimited},
		{"rate limit", 429, 20429, phoneVerification.RateLimited},
		{"invalid credentials", 401, 20003, phoneVerification.InvalidConfiguration},
		{"forbidden", 403, 20003, phoneVerification.InvalidConfiguration},
		{"invalid destination", 400, 60200, phoneVerification.Rejected},
		{"unknown server effect", 503, 20500, phoneVerification.Uncertain},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			p := loopbackProvider(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "42")
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": test.status, "code": test.code, "message": "provider leaked message " + syntheticPhone + " 123456 " + syntheticPassword, "more_info": "https://verify.twilio.com/private"})
			})
			result, err := p.Check(context.Background(), phoneVerification.CheckRequest{Binding: bindingFixture(), Code: "123456"})
			f := assertFailure(t, err, test.kind)
			if result.State == phoneVerification.Approved {
				t.Fatal("error approved verification")
			}
			if f.HTTPStatus != test.status || f.ProviderCode != test.code {
				t.Fatal("safe error metadata lost")
			}
			if test.status == 429 && f.RetryAfter != 42*time.Second {
				t.Fatal("retry advice lost")
			}
			if calls.Load() != 1 {
				t.Fatal("automatic retry")
			}
		})
	}
}

func TestTwilioMalformedOrMismatchedResponsesFailClosed(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"account":        func(r map[string]any) { r["account_sid"] = "ACffffffffffffffffffffffffffffffff" },
		"service":        func(r map[string]any) { r["service_sid"] = "VAffffffffffffffffffffffffffffffff" },
		"reference":      func(r map[string]any) { r["sid"] = "VEffffffffffffffffffffffffffffffff" },
		"phone":          func(r map[string]any) { r["to"] = "+525500000088" },
		"channel":        func(r map[string]any) { r["channel"] = "call" },
		"unknown status": func(r map[string]any) { r["status"] = "unknown" },
		"no timestamp":   func(r map[string]any) { delete(r, "date_created") },
	} {
		t.Run(name, func(t *testing.T) {
			r := resourceFixture("approved")
			mutate(r)
			p := loopbackProvider(t, func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(r) })
			_, err := p.Check(context.Background(), phoneVerification.CheckRequest{Binding: bindingFixture(), Code: "123456"})
			assertFailure(t, err, phoneVerification.Uncertain)
		})
	}
	approved := string(fixtureBytes(t, resourceFixture("approved")))
	for name, body := range map[string]string{"invalid json": "{", "trailing json": approved + "{}", "duplicate status": strings.TrimSuffix(approved, "}") + `,"status":"approved"}`, "oversized": strings.Repeat(" ", maxResponseBytes+1)} {
		t.Run(name, func(t *testing.T) {
			p := loopbackProvider(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			_, err := p.Check(context.Background(), phoneVerification.CheckRequest{Binding: bindingFixture(), Code: "123456"})
			assertFailure(t, err, phoneVerification.Uncertain)
		})
	}
	p := loopbackProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(resourceFixture("approved"))
	})
	_, err := p.Start(context.Background(), phoneVerification.StartRequest{DestinationE164: syntheticPhone, Channel: phoneVerification.SMS})
	assertFailure(t, err, phoneVerification.Uncertain)
}

func TestTwilioCancellationAndLostResponseRemainUncertain(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	p := loopbackProvider(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); close(entered); <-release })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.Start(ctx, phoneVerification.StartRequest{DestinationE164: syntheticPhone, Channel: phoneVerification.SMS})
		done <- err
	}()
	<-entered
	cancel()
	assertFailure(t, <-done, phoneVerification.Uncertain)
	if calls.Load() != 1 {
		t.Fatal("cancelled send repeated")
	}
	_, err := p.Start(ctx, phoneVerification.StartRequest{DestinationE164: syntheticPhone, Channel: phoneVerification.SMS})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancelled call lost cancellation")
	}
	if calls.Load() != 1 {
		t.Fatal("pre-cancelled call sent")
	}
	for _, method := range []string{"send", "fetch"} {
		t.Run(method, func(t *testing.T) {
			var received atomic.Int32
			p := loopbackProvider(t, func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
			})
			var err error
			if method == "send" {
				_, err = p.Start(context.Background(), phoneVerification.StartRequest{DestinationE164: syntheticPhone, Channel: phoneVerification.SMS})
				assertFailure(t, err, phoneVerification.Uncertain)
			} else {
				_, err = p.Fetch(context.Background(), bindingFixture())
				assertFailure(t, err, phoneVerification.Unavailable)
			}
			if received.Load() != 1 {
				t.Fatal("lost response retried")
			}
		})
	}
}

func TestTwilioConfiguredTimeoutIsBoundedAndUncertain(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); <-release }))
	defer server.Close()
	defer close(release)
	config := configFixture()
	config.RequestTimeout = 20 * time.Millisecond
	p, err := newTwilio(config, server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Start(context.Background(), phoneVerification.StartRequest{DestinationE164: syntheticPhone, Channel: phoneVerification.SMS})
	assertFailure(t, err, phoneVerification.Uncertain)
	if calls.Load() != 1 {
		t.Fatal("timeout send count changed")
	}
}

func TestTwilioRedirectsAndInvalidInputDoNotSendAgain(t *testing.T) {
	var redirected, requests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	p := loopbackProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	_, err := p.Start(context.Background(), phoneVerification.StartRequest{DestinationE164: syntheticPhone, Channel: phoneVerification.SMS})
	assertFailure(t, err, phoneVerification.Uncertain)
	if redirected.Load() != 0 {
		t.Fatal("followed redirect with credentials")
	}
	_, err = p.Start(context.Background(), phoneVerification.StartRequest{DestinationE164: "5500000099", Channel: phoneVerification.SMS})
	assertFailure(t, err, phoneVerification.InvalidInput)
	_, err = p.Start(context.Background(), phoneVerification.StartRequest{DestinationE164: syntheticPhone, Channel: "call"})
	assertFailure(t, err, phoneVerification.InvalidInput)
	_, err = p.Fetch(context.Background(), phoneVerification.Binding{Reference: "../../other", DestinationE164: syntheticPhone})
	assertFailure(t, err, phoneVerification.InvalidInput)
	_, err = p.Check(context.Background(), phoneVerification.CheckRequest{Binding: bindingFixture(), Code: "123"})
	assertFailure(t, err, phoneVerification.InvalidInput)
	if requests.Load() != 1 {
		t.Fatal("invalid request reached provider")
	}
}
