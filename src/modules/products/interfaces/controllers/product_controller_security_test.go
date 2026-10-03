package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
)

func TestAdjustStockRoute_OperatorCannotForgePaidPurchase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("product-adjust-route-security-with-enough-entropy")
	router := gin.New()
	NewProductController(nil, nil, nil, nil, nil, &prodApp.AdjustStock{}, tokens).RegisterRoutes(router)

	request := func(role, body string) int {
		t.Helper()
		token, err := tokens.GenerateAccessToken(uuid.New(), uuid.New(), role)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/products/"+uuid.NewString()+"/adjust-stock", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response.Code
	}

	paid := `{"movement_type":"restock","quantity":4,"cost":5,"is_purchase":true,"purchase_status":"paid","paid_on":"2026-08-24","payment_method":"transfer","paid_from":"gym_fund","idempotency_key":"forged-paid"}`
	if got := request("operator", paid); got != http.StatusForbidden {
		t.Fatalf("operator paid purchase status=%d, want 403", got)
	}
	implicitPaid := `{"movement_type":"restock","quantity":4,"cost":5,"is_purchase":true,"idempotency_key":"forged-default-paid"}`
	if got := request("operator", implicitPaid); got != http.StatusForbidden {
		t.Fatalf("operator default-paid purchase status=%d, want 403", got)
	}
	// Non-financial stock operations keep the existing Standard role policy;
	// malformed input reaches DTO validation rather than the financial guard.
	if got := request("operator", `{}`); got != http.StatusBadRequest {
		t.Fatalf("operator ordinary adjustment status=%d, want DTO validation 400", got)
	}
}

func TestRemotePurchaseRoutes_SeparateCloudPaymentFromLocalReceipt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("remote-purchase-role-and-surface-security")
	cloud := gin.New()
	NewProductController(nil, nil, nil, nil, nil, nil, tokens).WithRemotePurchases(&prodApp.CreateRemoteInventoryPurchase{}, nil).RegisterRoutes(cloud)
	desktop := gin.New()
	NewProductController(nil, nil, nil, nil, nil, nil, tokens).WithRemotePurchases(nil, &prodApp.ReceiveInventoryPurchase{}).RegisterRoutes(desktop)
	request := func(router *gin.Engine, path, role string) int {
		token, _ := tokens.GenerateAccessToken(uuid.New(), uuid.New(), role)
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	if got := request(cloud, "/api/v1/inventory-purchases", "operator"); got != 403 {
		t.Fatalf("operator forged remote payment: %d", got)
	}
	if got := request(desktop, "/api/v1/inventory-purchases", "owner"); got != 404 {
		t.Fatalf("remote creation exposed locally: %d", got)
	}
	if got := request(cloud, "/api/v1/inventory-purchases/"+uuid.NewString()+"/receive", "owner"); got != 404 {
		t.Fatalf("physical receipt exposed on cloud: %d", got)
	}
	if got := request(desktop, "/api/v1/inventory-purchases/"+uuid.NewString()+"/receive", "operator"); got != 400 {
		t.Fatalf("operator cannot reach receipt validation: %d", got)
	}
}
