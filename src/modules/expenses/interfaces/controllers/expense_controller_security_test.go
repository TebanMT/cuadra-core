package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
)

func TestExpenseRoutesAreStandardForOwnerAndRecurringStillRequiresPlus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("expenses-security-test-secret-with-enough-entropy")
	var gateCalls int32
	ctrl := &ExpenseController{
		Tokens: tokens,
		PlanGate: func(c *gin.Context) {
			atomic.AddInt32(&gateCalls, 1)
			c.AbortWithStatus(http.StatusPaymentRequired)
		},
	}
	r := gin.New()
	ctrl.RegisterRoutes(r)

	request := func(method, path, role string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		if role != "" {
			token, err := tokens.GenerateAccessToken(uuid.New(), uuid.New(), role)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	if got := request(http.MethodPost, "/api/v1/expenses", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d, want 401", got)
	}
	if got := request(http.MethodPost, "/api/v1/expenses", "operator").Code; got != http.StatusForbidden {
		t.Fatalf("operator status=%d, want 403", got)
	}
	if calls := atomic.LoadInt32(&gateCalls); calls != 0 {
		t.Fatalf("Plus gate called %d times before owner authorization", calls)
	}
	if got := request(http.MethodPost, "/api/v1/expenses", "owner").Code; got != http.StatusBadRequest {
		t.Fatalf("owner Standard status=%d, want DTO validation 400", got)
	}
	if calls := atomic.LoadInt32(&gateCalls); calls != 0 {
		t.Fatalf("Plus gate called for Standard expense route: %d", calls)
	}
	if got := request(http.MethodGet, "/api/v1/expense-templates", "owner").Code; got != http.StatusPaymentRequired {
		t.Fatalf("owner Standard recurring status=%d, want 402", got)
	}
}

func TestExpenseRoutesAllowOwnerPlusAndOperatorStandardOnlyForDrawerOperations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("expenses-role-matrix-secret-with-enough-entropy")
	var gateCalls int32
	ctrl := &ExpenseController{
		Tokens: tokens,
		PlanGate: func(c *gin.Context) {
			atomic.AddInt32(&gateCalls, 1)
		},
	}
	r := gin.New()
	ctrl.RegisterRoutes(r)

	request := func(method, path, role string) *httptest.ResponseRecorder {
		t.Helper()
		token, err := tokens.GenerateAccessToken(uuid.New(), uuid.New(), role)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// An owner reaches Standard expense DTO validation without the Plus gate.
	// validation. The intentionally empty body is rejected before a nil use
	// case can execute.
	if got := request(http.MethodPost, "/api/v1/expenses", "owner").Code; got != http.StatusBadRequest {
		t.Fatalf("owner Plus status=%d, want DTO validation 400", got)
	}
	if calls := atomic.LoadInt32(&gateCalls); calls != 0 {
		t.Fatalf("Plus gate calls=%d, want 0", calls)
	}

	// An operator reaches Standard drawer operations without touching the
	// Plus gate, but cannot classify the movement or enter Expenses.
	if got := request(http.MethodPost, "/api/v1/cash-movements", "operator").Code; got != http.StatusBadRequest {
		t.Fatalf("operator cash movement status=%d, want DTO validation 400", got)
	}
	if got := request(http.MethodPost, "/api/v1/expenses", "operator").Code; got != http.StatusForbidden {
		t.Fatalf("operator Expense status=%d, want 403", got)
	}
	if got := request(http.MethodPost, "/api/v1/cash-movements/"+uuid.NewString()+"/classify", "operator").Code; got != http.StatusForbidden {
		t.Fatalf("operator classify status=%d, want 403", got)
	}
	if got := request(http.MethodPatch, "/api/v1/cash-movements/"+uuid.NewString(), "operator").Code; got != http.StatusForbidden {
		t.Fatalf("operator cash correction status=%d, want 403", got)
	}
	if got := request(http.MethodDelete, "/api/v1/cash-movements/"+uuid.NewString()+"?version=1", "operator").Code; got != http.StatusForbidden {
		t.Fatalf("operator cash delete status=%d, want 403", got)
	}
	if calls := atomic.LoadInt32(&gateCalls); calls != 0 {
		t.Fatalf("Plus gate called for operator route; calls=%d", calls)
	}
}

func TestOperatorCanVoidOwnMovementButNotDeleteIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("expenses-void-test-secret-with-enough-entropy")
	r := gin.New()
	(&ExpenseController{Tokens: tokens, VoidCash: &expApp.VoidOwnCashMovement{}}).RegisterRoutes(r)
	token, err := tokens.GenerateAccessToken(uuid.New(), uuid.New(), "operator")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	id := uuid.New().String()
	// Reaches the handler (DTO validation) instead of the owner gate; the
	// use case decides whether the movement is theirs and the drawer open.
	if got := request(http.MethodPost, "/api/v1/cash-movements/"+id+"/void"); got != http.StatusBadRequest {
		t.Fatalf("operator void status=%d, want DTO validation 400", got)
	}
	if got := request(http.MethodDelete, "/api/v1/cash-movements/"+id+"?version=1"); got != http.StatusForbidden {
		t.Fatalf("operator delete status=%d, want 403", got)
	}
}
