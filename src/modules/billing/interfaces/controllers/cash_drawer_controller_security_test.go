package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/shared/auth"
)

func TestCashDrawerRoutePermissions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("cash-drawer-route-security-with-enough-entropy")
	router := gin.New()
	NewCashDrawerController(nil, tokens).RegisterRoutes(router)

	request := func(method, path, role, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if role != "" {
			token, err := tokens.GenerateAccessToken(uuid.New(), uuid.New(), role)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response.Code
	}

	valid := `{"name":"Recepción","idempotency_key":"drawer:reception"}`
	if got := request(http.MethodPost, "/api/v1/cash-drawers", "operator", valid); got != http.StatusForbidden {
		t.Fatalf("operator create status=%d, want 403", got)
	}
	if got := request(http.MethodPatch, "/api/v1/cash-drawers/"+uuid.NewString(), "operator", `{"name":"Otra","version":1}`); got != http.StatusForbidden {
		t.Fatalf("operator update status=%d, want 403", got)
	}
	if got := request(http.MethodDelete, "/api/v1/cash-drawers/"+uuid.NewString()+"?version=1", "operator", ""); got != http.StatusForbidden {
		t.Fatalf("operator deactivate status=%d, want 403", got)
	}
	if got := request(http.MethodPost, "/api/v1/cash-drawers", "owner", `{}`); got != http.StatusBadRequest {
		t.Fatalf("owner invalid create status=%d, want 400", got)
	}
	if got := request(http.MethodGet, "/api/v1/cash-drawers", "", ""); got != http.StatusUnauthorized {
		t.Fatalf("anonymous list status=%d, want 401", got)
	}
}
