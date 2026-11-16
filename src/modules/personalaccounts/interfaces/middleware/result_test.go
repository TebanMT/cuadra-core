//go:build server

package middleware

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestResultAccessSeparatesPersonalAndLimitedAuthority(t *testing.T) {
	tokens, bearer, actor := middlewareTokens(t)
	auth, err := NewPersonal(tokens, func() time.Time { return middlewareNow })
	if err != nil {
		t.Fatal(err)
	}
	access, err := NewResultAccess(auth)
	if err != nil {
		t.Fatal(err)
	}
	secret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	account, err := shared.WithAuthenticatedPersonalAccount(context.Background(), actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		headers http.Header
		ctx     context.Context
		status  int
		limited bool
	}{
		{"limited", http.Header{"X-Result-Secret": {secret}}, context.Background(), 200, true},
		{"personal", http.Header{"Authorization": {"Bearer " + bearer}}, context.Background(), 200, false},
		{"missing", http.Header{}, context.Background(), 401, false},
		{"mixed", http.Header{"Authorization": {"Bearer " + bearer}, "X-Result-Secret": {secret}}, context.Background(), 400, false},
		{"empty-present", http.Header{"Authorization": {"Bearer " + bearer}, "X-Result-Secret": {}}, context.Background(), 400, false},
		{"duplicate", http.Header{"X-Result-Secret": {secret}, "x-result-secret": {secret}}, context.Background(), 400, false},
		{"wrong-secret", http.Header{"X-Result-Secret": {"short"}}, context.Background(), 400, false},
		{"noncanonical", http.Header{"X-Result-Secret": {secret[:42] + "B"}}, context.Background(), 400, false},
		{"attempt", http.Header{"X-Attempt-Secret": {secret}, "X-Result-Secret": {secret}}, context.Background(), 400, false},
		{"inherited-account", http.Header{"X-Result-Secret": {secret}}, account, 400, false},
		{"inherited-gym", http.Header{"X-Result-Secret": {secret}}, shared.WithTenant(context.Background(), uuid.New()), 400, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			router := gin.New()
			router.GET("/auth/results/:id", access, func(c *gin.Context) {
				called = true
				value, limited := GetResultSecret(c.Request.Context())
				claims, personal := GetClaims(c.Request.Context())
				if limited != tc.limited || limited && (value != secret || personal || shared.ValidatePersonalAnonymousScope(c.Request.Context()) != nil) || !limited && (!personal || claims.AccountID != actor.AccountID) {
					t.Fatal("mixed or missing authority")
				}
				c.Status(200)
			})
			req := httptest.NewRequest("GET", "/auth/results/"+uuid.NewString(), nil).WithContext(tc.ctx)
			req.Header = tc.headers
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != tc.status || called != (tc.status == 200) {
				t.Fatalf("status=%d called=%v", response.Code, called)
			}
		})
	}
}
