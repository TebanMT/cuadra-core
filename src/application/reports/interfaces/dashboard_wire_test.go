package interfaces

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
)

func TestDashboardWire_OperatorOmitsAdministrativeMoneyAndKeepsDailyOperation(t *testing.T) {
	now := time.Date(2026, 8, 24, 18, 0, 0, 0, time.UTC)
	memberName := "Ana"
	margin := 50.0
	out := &reportsApp.DashboardOutput{
		GeneratedAt:             now,
		ActiveMembers:           reportsApp.KPI{Current: 8},
		IncomeMonth:             reportsApp.KPI{Current: 415},
		MembershipIncomeMonth:   reportsApp.KPI{Current: 400},
		ProductIncomeMonth:      reportsApp.KPI{Current: 15},
		OtherIncomeMonth:        reportsApp.KPI{Current: 25},
		UnclassifiedIncomeMonth: reportsApp.KPI{Current: 1},
		OperatingExpensesMonth:  reportsApp.KPI{Current: 1500},
		InventoryPurchasesMonth: reportsApp.KPI{Current: 50},
		RefundsMonth:            reportsApp.KPI{Current: 10},
		PeriodResultMonth:       reportsApp.KPI{Current: -1120},
		ExpensesMonth:           reportsApp.KPI{Current: 1560},
		Integrity:               reportsApp.FinancialIntegrity{Status: "incomplete", Warnings: []string{"test"}},
		RealizedProfitMonth:     &reportsApp.KPI{Current: 7},
		RealizedProfitCoverage:  &reportsApp.ProfitCoverage{ItemsWithCost: 1, ItemsTotal: 1},
		RealizedProfitMarginPct: &margin,
		IncomeLast30Days:        []reportsApp.DailyIncome{{Date: now, Total: 415}},
		CheckinsToday:           3,
		ExpiringThisWeek:        2,
		RecoverableExpired:      1,
		TodayCash:               map[string]float64{"cash": 15},
		TodayCashTotal:          15,
		RecentPayments: []reportsApp.RecentPaymentRow{{
			ID: uuid.New(), MemberName: &memberName, Amount: 15,
			Method: "cash", Concept: "product", PaymentDate: now,
		}},
	}

	payload, err := json.Marshal(toDashboardWire(out, false))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err = json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"income_month", "membership_income_month", "product_income_month", "other_income_month",
		"unclassified_income_month", "operating_expenses_month", "inventory_purchases_month",
		"refunds_month", "period_result_month", "expenses_month", "integrity",
		"realized_profit_month", "realized_profit_coverage", "realized_profit_margin_pct",
	} {
		if _, present := wire[key]; present {
			t.Fatalf("operator wire leaked %q: %s", key, payload)
		}
	}
	if series, ok := wire["income_30d"].([]any); !ok || len(series) != 0 {
		t.Fatalf("operator income_30d=%#v, want explicit empty array", wire["income_30d"])
	}
	if wire["checkins_today"] != float64(3) || wire["active_members"] == nil || wire["attention_summary"] == nil {
		t.Fatalf("operator daily operation missing: %s", payload)
	}
	cash, ok := wire["cash_today"].(map[string]any)
	if !ok || cash["total"] != float64(15) {
		t.Fatalf("operator cash_today=%#v, want operational $15", wire["cash_today"])
	}
	recent, ok := wire["recent_payments"].([]any)
	if !ok || len(recent) != 1 || recent[0].(map[string]any)["amount"] != float64(15) {
		t.Fatalf("operator recent_payments=%#v, want operational collection", wire["recent_payments"])
	}
}
