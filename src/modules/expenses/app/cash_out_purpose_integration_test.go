//go:build sidecar

package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	repos "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	controllers "github.com/cuadra/cuadra-core/src/modules/expenses/interfaces/controllers"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/gin-gonic/gin"
)

func TestCashOutPurposeHTTP(t *testing.T) {
	for _, role := range []string{"operator", "owner"} {
		t.Run(role, func(t *testing.T) {
			f := setupExpenses(t)
			if _, err := f.db.Exec(`UPDATE users SET role=? WHERE id=?`, role, f.ownerID.String()); err != nil {
				t.Fatal(err)
			}
			tokens := auth.NewJWTService("cash-out-purpose-test-secret")
			token, err := tokens.GenerateAccessToken(f.ownerID, f.gymID, role)
			if err != nil {
				t.Fatal(err)
			}
			uc := expApp.NewCreateCashMovement(repos.NewCashMovementSQLiteRepository(), f.uow, f.recorder, expApp.OperationalPolicy{}).WithExpenses(f.expenseRepo)
			gin.SetMode(gin.TestMode)
			router := gin.New()
			(&controllers.ExpenseController{Tokens: tokens, CreateCash: uc}).RegisterRoutes(router)
			request := func(path, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+token)
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				return res
			}
			body := `{"movement_on":"2026-09-01","amount":19.99,"movement_type":"cash_out","reason":"Limpieza","purpose":"expense","category":"servicios","idempotency_key":"once","payment_method":"transfer","paid_from":"external"}`
			var identity string
			for attempt := 0; attempt < 2; attempt++ {
				res := request("/api/v1/cash-movements", body)
				if res.Code != 201 {
					t.Fatalf("status=%d body=%s", res.Code, res.Body)
				}
				var wire struct {
					Data struct {
						ID        string `json:"id"`
						Status    string `json:"classification_status"`
						ExpenseID string `json:"expense_id"`
					} `json:"data"`
				}
				if err := json.Unmarshal(res.Body.Bytes(), &wire); err != nil {
					t.Fatal(err)
				}
				if wire.Data.Status != "expense" || wire.Data.ExpenseID == "" {
					t.Fatalf("missing expense: %s", res.Body)
				}
				if attempt == 0 {
					identity = wire.Data.ID
				} else if wire.Data.ID != identity {
					t.Fatal("retry created another outflow")
				}
			}
			var expenses, movements, amount int
			if err := f.db.Get(&expenses, `SELECT COUNT(*) FROM expenses WHERE payment_method='cash' AND paid_from='cash_register' AND category='servicios'`); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRowx(`SELECT COUNT(*),SUM(amount) FROM cash_movements`).Scan(&movements, &amount); err != nil {
				t.Fatal(err)
			}
			if expenses != 1 || movements != 1 || amount != 1999 {
				t.Fatalf("expenses=%d movements=%d cents=%d", expenses, movements, amount)
			}
			// Both sides carry the durable sync group; a small batch cannot split them.
			var payloads []string
			if err := f.db.Select(&payloads, `SELECT payload FROM sync_queue WHERE entity_type IN ('expenses','cash_movements')`); err != nil {
				t.Fatal(err)
			}
			for _, payload := range payloads {
				var data map[string]any
				if err := json.Unmarshal([]byte(payload), &data); err != nil {
					t.Fatal(err)
				}
				members, ok := data["_sync_expense_members"].([]any)
				if !ok || len(members) != 2 {
					t.Fatalf("incomplete sync group: %s", payload)
				}
			}
			changed := request("/api/v1/cash-movements", strings.Replace(body, `"servicios"`, `"renta"`, 1))
			if changed.Code < 400 || changed.Code >= 500 {
				t.Fatalf("changed retry status=%d", changed.Code)
			}
			delivered := request("/api/v1/cash-movements", `{"movement_on":"2026-09-01","amount":30,"movement_type":"cash_out","reason":"Entrega al dueño","purpose":"non_operating","idempotency_key":"delivery"}`)
			if delivered.Code != 201 || !strings.Contains(delivered.Body.String(), `"classification_status":"non_operating"`) {
				t.Fatalf("delivery: %d %s", delivered.Code, delivered.Body)
			}
			var pending int
			if err := f.db.Get(&pending, `SELECT COUNT(*) FROM cash_movements WHERE classification_status='unclassified'`); err != nil {
				t.Fatal(err)
			}
			if pending != 0 {
				t.Fatal("delivery left a classification task")
			}
			if err := f.db.Get(&expenses, `SELECT COUNT(*) FROM expenses`); err != nil {
				t.Fatal(err)
			}
			if expenses != 1 {
				t.Fatal("delivery created an expense")
			}
			if role == "operator" {
				for _, path := range []string{"/api/v1/expenses", "/api/v1/cash-movements/" + identity + "/classify"} {
					if res := request(path, body); res.Code != 403 {
						t.Fatalf("operator gained admin permission %s: %d", path, res.Code)
					}
				}
			}
		})
	}
}

func TestCashOutPurposeValidationAndRollback(t *testing.T) {
	for _, tc := range []struct {
		name, purpose, category, kind string
		failAudit                     bool
	}{
		{"missing category", "expense", "", "cash_out", false},
		{"bad category", "expense", "inventory", "cash_out", false},
		{"bad purpose", "inventory_purchase", "", "cash_out", false},
		{"cash in expense", "expense", "servicios", "cash_in", false},
		{"category on delivery", "non_operating", "servicios", "cash_out", false},
		{"audit failure", "expense", "servicios", "cash_out", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupExpenses(t)
			recorder := f.recorder
			if tc.failAudit {
				recorder = failingExpenseAudit{}
			}
			uc := expApp.NewCreateCashMovement(repos.NewCashMovementSQLiteRepository(), f.uow, recorder, expApp.OperationalPolicy{}).WithExpenses(f.expenseRepo)
			_, err := uc.Execute(context.Background(), expApp.CashMovementInput{GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: mexicoBusinessDay(t), Amount: 20, MovementType: tc.kind, Reason: "Salida", Purpose: tc.purpose, Category: tc.category, IdempotencyKey: "invalid"})
			if err == nil {
				t.Fatal("expected rejection")
			}
			for _, table := range []string{"expenses", "cash_movements"} {
				var n int
				if err := f.db.Get(&n, "SELECT COUNT(*) FROM "+table); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Fatalf("partial write in %s", table)
				}
			}
			var n int
			if err := f.db.Get(&n, `SELECT COUNT(*) FROM sync_queue WHERE entity_type IN ('expenses','cash_movements')`); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatal("partial sync command")
			}
		})
	}
}

func TestCashOutLegacyStillWaitsForClassification(t *testing.T) {
	f := setupExpenses(t)
	uc := expApp.NewCreateCashMovement(repos.NewCashMovementSQLiteRepository(), f.uow, f.recorder, expApp.OperationalPolicy{})
	m, err := uc.Execute(context.Background(), expApp.CashMovementInput{GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: mexicoBusinessDay(t), Amount: 20, MovementType: cashDomain.CashOut, Reason: "Salida antigua", IdempotencyKey: "legacy"})
	if err != nil || m.ClassificationStatus != cashDomain.Unclassified {
		t.Fatalf("legacy changed: %+v %v", m, err)
	}
}
