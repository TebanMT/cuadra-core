//go:build sidecar

package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	billingControllers "github.com/cuadra/cuadra-core/src/modules/billing/interfaces/controllers"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
)

// "Ver totales de dinero" (UC-007): without it an operator lists today's
// payments only and gets no totals; the owner and internal callers are
// unchanged.
func TestViewTotals_PaymentsList(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	registerCashProductSale(t, f, productID)
	today := cashSessionBusinessDay(t)
	list := billingApp.NewListGymPayments(f.paymentRepo, f.memberRepo, f.uow).WithGyms(f.gymRepo)

	hidden, err := list.Execute(operatorCtx(), billingApp.ListGymPaymentsInput{GymID: f.gymID})
	if err != nil {
		t.Fatalf("operator today (default range): %v", err)
	}
	if !hidden.TotalsHidden || len(hidden.Items) != 1 || hidden.TotalPaid != 0 || hidden.CashTotal != 0 {
		t.Errorf("operator today: hidden=%v items=%d total=%v cash=%v, want the rows without totals",
			hidden.TotalsHidden, len(hidden.Items), hidden.TotalPaid, hidden.CashTotal)
	}
	if _, err := list.Execute(operatorCtx(), billingApp.ListGymPaymentsInput{GymID: f.gymID, From: today, To: today}); err != nil {
		t.Errorf("operator explicit today: %v", err)
	}

	for name, in := range map[string]billingApp.ListGymPaymentsInput{
		"yesterday":    {GymID: f.gymID, From: today.AddDate(0, 0, -1), To: today.AddDate(0, 0, -1)},
		"month so far": {GymID: f.gymID, From: today.AddDate(0, 0, -10), To: today},
		"only to set":  {GymID: f.gymID, To: today.AddDate(0, 0, -2)},
	} {
		_, err := list.Execute(operatorCtx(sharedDomain.PermCredit), in)
		t.Run(name, func(t *testing.T) { assertDenied(t, err, sharedDomain.PermViewTotals) })
	}

	for name, ctx := range map[string]context.Context{
		"operator with view_totals": operatorCtx(sharedDomain.PermViewTotals),
		"owner":                     sharedDomain.WithGrants(context.Background(), sharedDomain.OwnerGrants()),
		"internal":                  context.Background(),
	} {
		out, err := list.Execute(ctx, billingApp.ListGymPaymentsInput{GymID: f.gymID, From: today.AddDate(0, 0, -10), To: today})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if out.TotalsHidden || out.TotalPaid != 20 || out.CashTotal != 20 {
			t.Errorf("%s: hidden=%v total=%v cash=%v, want 20 in cash", name, out.TotalsHidden, out.TotalPaid, out.CashTotal)
		}
	}
}

func TestViewTotals_CashCloseReport(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	registerCashProductSale(t, f, productID)
	today := cashSessionBusinessDay(t)
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).WithGyms(f.gymRepo)

	out, err := cash.Report(operatorCtx(), reportsApp.CashCloseReportInput{GymID: f.gymID})
	if err != nil {
		t.Fatalf("operator today: %v", err)
	}
	if !out.TotalsHidden {
		t.Error("operator without view_totals: TotalsHidden = false")
	}
	if _, err := cash.Report(operatorCtx(), reportsApp.CashCloseReportInput{GymID: f.gymID, Date: today}); err != nil {
		t.Errorf("operator explicit today: %v", err)
	}
	_, err = cash.Report(operatorCtx(sharedDomain.PermCashOut), reportsApp.CashCloseReportInput{GymID: f.gymID, Date: today.AddDate(0, 0, -1)})
	assertDenied(t, err, sharedDomain.PermViewTotals)

	for name, ctx := range map[string]context.Context{
		"operator with view_totals": operatorCtx(sharedDomain.PermViewTotals),
		"owner":                     sharedDomain.WithGrants(context.Background(), sharedDomain.OwnerGrants()),
	} {
		out, err := cash.Report(ctx, reportsApp.CashCloseReportInput{GymID: f.gymID, Date: today.AddDate(0, 0, -1)})
		if err != nil || out.TotalsHidden {
			t.Errorf("%s yesterday: hidden=%v err=%v", name, out != nil && out.TotalsHidden, err)
		}
		out, err = cash.Report(ctx, reportsApp.CashCloseReportInput{GymID: f.gymID})
		if err != nil || out.TotalsHidden || out.Totals.GrandTotal != 20 {
			t.Errorf("%s today: out=%+v err=%v, want totals of 20", name, out, err)
		}
	}
}

// End to end through the real middleware: an operator token on a GET
// carries the operator's grants, and the money totals are absent from the
// JSON (not zero) while the drawer figures stay.
func TestViewTotals_HTTPResponsesOmitTotals(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	registerCashProductSale(t, f, productID)

	users := usersRepoLite.NewUserSQLiteRepository()
	tokens := auth.NewJWTService("view-totals-test-secret-with-enough-entropy")
	seedOperator := func(phone string, perms ...sharedDomain.Permission) string {
		t.Helper()
		now := time.Now().UTC()
		u, err := userDomain.NewOperator(uuid.New(), f.gymID, "Recepción", phone, nil, "", f.ownerID, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := u.SetPermissions(perms, now); err != nil {
			t.Fatal(err)
		}
		if err := f.uow.Command(sharedDomain.WithTenant(context.Background(), f.gymID), func(tx sharedDomain.Transaction) error {
			_, err := users.Create(tx, u)
			return err
		}); err != nil {
			t.Fatalf("seed operator: %v", err)
		}
		tok, err := tokens.GenerateAccessToken(u.ID, f.gymID, "operator")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	ownerTok, err := tokens.GenerateAccessToken(f.ownerID, f.gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	reception := seedOperator("+524421234567", sharedDomain.PermCredit)
	trusted := seedOperator("+524427654321", sharedDomain.PermViewTotals)

	r := gin.New()
	r.Use(middleware.EnforceOperatorPermissions(tokens, users, f.uow))
	ctrl := &billingControllers.PaymentController{
		Tokens:    tokens,
		ListByGym: billingApp.NewListGymPayments(f.paymentRepo, f.memberRepo, f.uow).WithGyms(f.gymRepo),
		CashClose: reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).WithGyms(f.gymRepo),
	}
	ctrl.RegisterRoutes(r)
	get := func(path, tok string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		body := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if data, ok := body["data"].(map[string]any); ok {
			body = data
		}
		return rec.Code, body
	}

	paymentTotals := []string{"total_paid", "refund_total", "cash_total", "transfer_total", "card_total"}
	closeTotals := []string{"by_method", "by_concept", "operators", "total", "refunds_total", "refunds_count", "refund_by_method"}

	code, body := get("/api/v1/payments", reception)
	if code != http.StatusOK {
		t.Fatalf("operator payments today = %d %v", code, body)
	}
	for _, k := range paymentTotals {
		if _, present := body[k]; present {
			t.Errorf("operator payments leaked %q", k)
		}
	}
	yesterday := cashSessionBusinessDay(t).AddDate(0, 0, -1).Format("2006-01-02")
	if code, body := get("/api/v1/payments?from="+yesterday+"&to="+yesterday, reception); code != http.StatusForbidden ||
		body["exception"] != sharedDomain.PermissionDeniedMessage(sharedDomain.PermViewTotals) {
		t.Errorf("operator payments yesterday = %d %v, want 403 with the view-totals message", code, body)
	}

	code, body = get("/api/v1/cash-close", reception)
	if code != http.StatusOK {
		t.Fatalf("operator cash close = %d %v", code, body)
	}
	for _, k := range closeTotals {
		if _, present := body[k]; present {
			t.Errorf("operator cash close leaked %q", k)
		}
	}
	if _, present := body["cash_movements"]; !present {
		t.Error("operator cash close lost the drawer movements")
	}
	if code, _ := get("/api/v1/cash-close?date="+yesterday, reception); code != http.StatusForbidden {
		t.Errorf("operator cash close yesterday = %d, want 403", code)
	}

	for name, tok := range map[string]string{"owner": ownerTok, "operator with view_totals": trusted} {
		code, body := get("/api/v1/payments", tok)
		if code != http.StatusOK || body["total_paid"] != float64(20) {
			t.Errorf("%s payments = %d total_paid=%v, want 20", name, code, body["total_paid"])
		}
		code, body = get("/api/v1/cash-close", tok)
		if code != http.StatusOK || body["total"] != float64(20) || body["refunds_count"] != float64(0) {
			t.Errorf("%s cash close = %d total=%v refunds_count=%v, want 20 and an explicit 0", name, code, body["total"], body["refunds_count"])
		}
		if code, _ := get("/api/v1/cash-close?date="+yesterday, tok); code != http.StatusOK {
			t.Errorf("%s cash close yesterday = %d", name, code)
		}
	}
}
