//go:build server

package middleware

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/sidecartoken"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var middlewareNow = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

func middlewareTokens(t *testing.T) (security.AccessTokens, string, security.AccessClaims) {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := crypto.NewAccessTokens("personal_test", private, map[string]ed25519.PublicKey{"personal_test": pub})
	if err != nil {
		t.Fatal(err)
	}
	token, claims, err := tokens.Issue(uuid.New(), uuid.New(), uuid.New(), middlewareNow)
	if err != nil {
		t.Fatal(err)
	}
	return tokens, token, claims
}
func middlewareRouter(t *testing.T, tokens security.AccessTokens, now time.Time, next gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	middleware, err := NewPersonal(tokens, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(middleware)
	r.GET("/me", next)
	return r
}
func assertAuthRequired(t *testing.T, response *httptest.ResponseRecorder, private ...string) {
	t.Helper()
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected401, got %d", response.Code)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
		t.Fatal("authentication response cacheable")
	}
	var envelope map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope) != 6 || envelope["code"] != "AUTH_REQUIRED" || envelope["action"] != "authenticate" || envelope["retryable"] != false {
		t.Fatal("invalid authentication error envelope")
	}
	fields, ok := envelope["fields"].([]any)
	if !ok || len(fields) != 0 {
		t.Fatal("fields must be empty array")
	}
	id, err := uuid.Parse(response.Header().Get("X-Request-ID"))
	if err != nil || id.Version() != 4 || envelope["request_id"] != id.String() {
		t.Fatal("request ID absent or not randomUUID")
	}
	for _, secret := range private {
		if secret != "" && strings.Contains(response.Body.String(), secret) {
			t.Fatal("private request detail exposed")
		}
	}
}

func TestPersonalMiddlewareStoresTypedRequestClaimsAfterVerification(t *testing.T) {
	tokens, encoded, want := middlewareTokens(t)
	called := false
	r := middlewareRouter(t, tokens, middlewareNow, func(c *gin.Context) {
		called = true
		got, ok := GetClaims(c.Request.Context())
		if !ok || got != want {
			t.Error("verified claims absent from request context")
		}
		if shared.PersonalAccountID(c.Request.Context()) != want.AccountID {
			t.Error("personal owner not scoped")
		}
		if _, ok = c.Get("claims"); ok {
			t.Error("claims exposed as untyped Gin value")
		}
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest("GET", "/me?limit=20", nil)
	request.Header.Set("Authorization", "Bearer "+encoded)
	request.Header.Set("X-Request-ID", "caller-controlled-id")
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)
	if !called || response.Code != http.StatusNoContent || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Request-ID") == "caller-controlled-id" {
		t.Fatal("valid request not accepted safely")
	}
	if _, ok := GetClaims(request.Context()); ok {
		t.Fatal("original request context mutated")
	}
	if _, ok := GetClaims(context.WithValue(context.Background(), "claims", want)); ok {
		t.Fatal("untyped claims trusted")
	}
	if _, ok := GetClaims(nil); ok {
		t.Fatal("nil context has claims")
	}
}

func TestPersonalMiddlewareRejectsAmbiguousOrAlternativeCredentials(t *testing.T) {
	tokens, encoded, _ := middlewareTokens(t)
	for name, change := range map[string]func(*http.Request){
		"missing": func(r *http.Request) { r.Header.Del("Authorization") }, "empty": func(r *http.Request) { r.Header.Set("Authorization", "") }, "basic": func(r *http.Request) { r.Header.Set("Authorization", "Basic "+encoded) }, "lowercase scheme": func(r *http.Request) { r.Header.Set("Authorization", "bearer "+encoded) }, "double space": func(r *http.Request) { r.Header.Set("Authorization", "Bearer  "+encoded) }, "tab": func(r *http.Request) { r.Header.Set("Authorization", "Bearer\t"+encoded) }, "trailing space": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+encoded+" ") }, "leading space": func(r *http.Request) { r.Header.Set("Authorization", " Bearer "+encoded) }, "duplicate": func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+encoded) }, "case duplicate": func(r *http.Request) { r.Header["authorization"] = []string{"Bearer " + encoded} }, "combined": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+encoded+", Bearer "+encoded) }, "overlong": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 511)+".x.x") }, "query only": func(r *http.Request) {
			r.Header.Del("Authorization")
			r.URL.RawQuery = "access_token=" + url.QueryEscape(encoded)
		}, "query with header": func(r *http.Request) { r.URL.RawQuery = "access_token=" + url.QueryEscape(encoded) }, "query mixed case": func(r *http.Request) { r.URL.RawQuery = "%54oKen=" + url.QueryEscape(encoded) }, "query authorization": func(r *http.Request) { r.URL.RawQuery = "Authorization=" + url.QueryEscape("Bearer "+encoded) }, "malformed query": func(r *http.Request) { r.URL.RawQuery = "token=%ZZ" }, "cookie only": func(r *http.Request) {
			r.Header.Del("Authorization")
			r.AddCookie(&http.Cookie{Name: "access_token", Value: encoded})
		},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			r := middlewareRouter(t, tokens, middlewareNow, func(*gin.Context) { called = true })
			request := httptest.NewRequest("GET", "/me", nil)
			request.Header.Set("Authorization", "Bearer "+encoded)
			change(request)
			response := httptest.NewRecorder()
			r.ServeHTTP(response, request)
			assertAuthRequired(t, response, encoded)
			if called {
				t.Fatal("unauthorized request reached handler")
			}
		})
	}
}

func TestPersonalMiddlewareRejectsRealAdministrativeAndSidecarCredentials(t *testing.T) {
	tokens, _, _ := middlewareTokens(t)
	admin := auth.NewJWTService("synthetic-admin-key-for-cross-scope-test")
	adminAccess, err := admin.GenerateAccessToken(uuid.New(), uuid.New(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.ValidateAccessToken(adminAccess); err != nil {
		t.Fatal("administrative fixture must be valid in its own realm", err)
	}
	adminRefresh, err := admin.GenerateRefreshToken(uuid.New(), uuid.New(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	sidecarJWT, err := auth.NewJWTServiceWithDurations("synthetic-sidecar-key", 365*24*time.Hour, 365*24*time.Hour).GenerateAccessToken(uuid.New(), uuid.New(), "operator")
	if err != nil {
		t.Fatal(err)
	}
	opaque, _, err := sidecartoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"admin access": adminAccess, "admin refresh": adminRefresh, "sidecar JWT": sidecarJWT, "sidecar opaque": opaque} {
		t.Run(name, func(t *testing.T) {
			r := middlewareRouter(t, tokens, middlewareNow, func(*gin.Context) { t.Error("cross-scope credential reached handler") })
			request := httptest.NewRequest("GET", "/me", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			r.ServeHTTP(response, request)
			assertAuthRequired(t, response, token)
		})
	}
}

func TestPersonalMiddlewareRejectsTamperedExpiredAndWrongSigningKey(t *testing.T) {
	tokens, encoded, _ := middlewareTokens(t)
	other, _, _ := middlewareTokens(t)
	parts := strings.Split(encoded, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	signature[0] ^= 1
	parts[2] = base64.RawURLEncoding.EncodeToString(signature)
	tampered := strings.Join(parts, ".")
	for name, caseData := range map[string]struct {
		tokens security.AccessTokens
		token  string
		now    time.Time
	}{"tampered": {tokens, tampered, middlewareNow}, "expired": {tokens, encoded, middlewareNow.Add(security.AccessTTL + security.AccessClockSkew)}, "wrong key": {other, encoded, middlewareNow}} {
		t.Run(name, func(t *testing.T) {
			r := middlewareRouter(t, caseData.tokens, caseData.now, func(*gin.Context) { t.Error("invalid JWT reached handler") })
			request := httptest.NewRequest("GET", "/me", nil)
			request.Header.Set("Authorization", "Bearer "+caseData.token)
			response := httptest.NewRecorder()
			r.ServeHTTP(response, request)
			assertAuthRequired(t, response, caseData.token)
		})
	}
}

type middlewareFailureTokens struct {
	security.AccessTokens
	err error
}

func (f middlewareFailureTokens) Verify(string, time.Time) (security.AccessClaims, error) {
	return security.AccessClaims{}, f.err
}
func TestPersonalMiddlewareRedactsVerifierFailureAndRejectsMissingConfig(t *testing.T) {
	if _, err := NewPersonal(nil, time.Now); !errors.Is(err, personalErrors.ErrInvalidSecurityConfiguration) {
		t.Fatal(err)
	}
	tokens, encoded, _ := middlewareTokens(t)
	if _, err := NewPersonal(tokens, nil); !errors.Is(err, personalErrors.ErrInvalidSecurityConfiguration) {
		t.Fatal(err)
	}
	private := "provider internal private detail"
	r := middlewareRouter(t, middlewareFailureTokens{err: errors.New(private)}, middlewareNow, func(*gin.Context) { t.Error("failure reached handler") })
	ids := map[string]bool{}
	for i := 0; i < 3; i++ {
		request := httptest.NewRequest("GET", "/me", nil)
		request.Header.Set("Authorization", "Bearer "+encoded)
		response := httptest.NewRecorder()
		r.ServeHTTP(response, request)
		assertAuthRequired(t, response, encoded, private)
		id := response.Header().Get("X-Request-ID")
		if ids[id] {
			t.Fatal("request ID reused")
		}
		ids[id] = true
	}
}
