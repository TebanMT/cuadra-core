//go:build server && integration

package main

import (
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMancuernaMountedWithRestrictedPoolsAndNoImplicitProviderCall(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := syntheticMancuernaConfig(t)
	for name, target := range map[string]*string{"MANCUERNA_TEST_AUTH_DSN": &c.AuthDatabaseURL, "MANCUERNA_TEST_DATA_DSN": &c.DataDatabaseURL} {
		value := os.Getenv(name)
		if value == "" {
			t.Skip("explicit local PostgreSQL fixture required")
		}
		parsed, err := url.Parse(value)
		if err != nil || !strings.Contains(parsed.Path, "mancuerna") || !strings.Contains(parsed.Path, "test") || (parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1") {
			t.Fatal("refusing non-local test database")
		}
		*target = value
	}
	r := newServerRouter()
	close, err := mountMancuerna(r, c)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	expected := expectedMancuernaRoutes()
	for _, route := range r.Routes() {
		if _, ok := expected[route.Method+" "+route.Path]; !ok {
			t.Fatal("unexpected personal route", route.Method, route.Path)
		}
		expected[route.Method+" "+route.Path] = true
	}
	for _, path := range []string{"/exercises", "/routines", "/routines/10000000-0000-4000-8000-000000000001", "/routines/10000000-0000-4000-8000-000000000001/draft", "/routine-versions/10000000-0000-4000-8000-000000000001"} {
		response := httptest.NewRecorder()
		r.ServeHTTP(response, httptest.NewRequest("GET", "/api/mancuerna/v1"+path, nil))
		if response.Code != 401 {
			t.Fatal("routine reader missing or anonymous request authorized", path, response.Code)
		}
	}
	for route, found := range expected {
		if !found {
			t.Fatal("route not mounted", route)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/mancuerna/v1/me", nil))
	if w.Code != 401 {
		t.Fatal("anonymous Me authorized", w.Code)
	}
	// Missing body must be rejected before contacting even a configured provider.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/mancuerna/v1/auth/start", nil))
	if w.Code != 400 {
		t.Fatal("invalid start was not rejected locally", w.Code)
	}
	swapped := c
	swapped.AuthDatabaseURL, swapped.DataDatabaseURL = c.DataDatabaseURL, c.AuthDatabaseURL
	if close, err := mountMancuerna(gin.New(), swapped); err == nil {
		close()
		t.Fatal("auth/data pool swap accepted")
	}
}

func expectedMancuernaRoutes() map[string]bool {
	return map[string]bool{
		"POST /api/mancuerna/v1/auth/start":                                          false,
		"POST /api/mancuerna/v1/auth/recovery/complete":                              false,
		"POST /api/mancuerna/v1/auth/check":                                          false,
		"GET /api/mancuerna/v1/auth/attempts/:id":                                    false,
		"POST /api/mancuerna/v1/auth/attempts/:id/resend":                            false,
		"POST /api/mancuerna/v1/auth/refresh":                                        false,
		"POST /api/mancuerna/v1/auth/logout":                                         false,
		"GET /api/mancuerna/v1/auth/results/:id":                                     false,
		"GET /api/mancuerna/v1/me":                                                   false,
		"POST /api/mancuerna/v1/me/recovery-key/confirm":                             false,
		"POST /api/mancuerna/v1/me/proofs":                                           false,
		"POST /api/mancuerna/v1/me/proofs/:id/complete":                              false,
		"POST /api/mancuerna/v1/me/recovery-key":                                     false,
		"POST /api/mancuerna/v1/me/phone-change/start":                               false,
		"POST /api/mancuerna/v1/me/phone-change/complete":                            false,
		"POST /api/mancuerna/v1/me/deletion":                                         false,
		"POST /api/mancuerna/v1/commands":                                            false,
		"GET /api/mancuerna/v1/exercises":                                            false,
		"GET /api/mancuerna/v1/routines":                                             false,
		"GET /api/mancuerna/v1/routines/:id":                                         false,
		"GET /api/mancuerna/v1/routines/:id/draft":                                   false,
		"GET /api/mancuerna/v1/routine-versions/:id":                                 false,
		"POST /api/mancuerna/v1/sync/push":                                           false,
		"POST /api/mancuerna/v1/me/vaults":                                           false,
		"POST /api/mancuerna/v1/me/vaults/:id/unlock":                                false,
		"GET /api/mancuerna/v1/sync/pull":                                            false,
		"GET /api/mancuerna/v1/me/devices":                                           false,
		"POST /api/mancuerna/v1/me/devices/:id/revoke":                               false,
		"POST /api/mancuerna/v1/me/devices/revoke":                                   false,
		"POST /api/mancuerna/v1/auth/device-authorizations":                          false,
		"GET /api/mancuerna/v1/me/device-authorizations":                             false,
		"POST /api/mancuerna/v1/me/device-authorizations/:id/decision":               false,
		"GET /api/mancuerna/v1/me/device-authorizations/:id/decisions/:operation_id": false,
		"POST /api/mancuerna/v1/sync/snapshots":                                      false,
		"GET /api/mancuerna/v1/sync/snapshots/:id/pages/:number":                     false,
	}
}
