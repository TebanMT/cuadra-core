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

func TestCashSessionRoutePermissions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("cash-session-route-security-with-enough-entropy")
	ctrl := &PaymentController{Tokens: tokens}
	router := gin.New()
	ctrl.RegisterRoutes(router)

	request := func(method, path, role, body string) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if role != "" {
			token, err := tokens.GenerateAccessToken(uuid.New(), uuid.New(), role)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	validReopen := `{"date":"2026-08-24","reason":"conteo capturado por error"}`
	if got := request(http.MethodPost, "/api/v1/cash-close/reopen", "operator", validReopen); got != http.StatusForbidden {
		t.Fatalf("operator reopen status=%d, want 403", got)
	}
	if got := request(http.MethodPost, "/api/v1/cash-sessions/"+uuid.NewString()+"/reopen", "operator", `{"reason":"conteo incorrecto"}`); got != http.StatusForbidden {
		t.Fatalf("operator session reopen status=%d, want 403", got)
	}
	if got := request(http.MethodPost, "/api/v1/cash-close/reopen", "owner", `{}`); got != http.StatusBadRequest {
		t.Fatalf("owner reopen without reason status=%d, want 400", got)
	}
	if got := request(http.MethodPost, "/api/v1/cash-sessions/not-a-uuid/reconcile", "operator", `{}`); got != http.StatusBadRequest {
		t.Fatalf("operator reconcile status=%d, want handler-level 400", got)
	}
	if got := request(http.MethodPost, "/api/v1/cash-sessions/not-a-uuid/withdraw", "operator", `{}`); got != http.StatusBadRequest {
		t.Fatalf("operator withdraw status=%d, want handler-level 400", got)
	}
	if got := request(http.MethodPost, "/api/v1/cash-close/reopen", "", validReopen); got != http.StatusUnauthorized {
		t.Fatalf("anonymous reopen status=%d, want 401", got)
	}
}
