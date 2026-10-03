package reports

import (
	"testing"
	"time"
)

func TestPreviousMonthMTDEnd(t *testing.T) {
	cases := []struct {
		today time.Time
		want  string
	}{
		{time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), "2026-07-12"},
		{time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC), "2026-02-28"},
		{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "2025-12-01"},
	}
	for _, tc := range cases {
		if got := previousMonthMTDEnd(tc.today).Format("2006-01-02"); got != tc.want {
			t.Errorf("previousMonthMTDEnd(%s) = %s, want %s", tc.today.Format("2006-01-02"), got, tc.want)
		}
	}
}

func TestFinancialKPIsUseCentSafeArithmetic(t *testing.T) {
	snapshot := CanonicalFinancialSnapshot{
		MembershipIncome:   0.10,
		ProductIncome:      0.20,
		OperatingExpenses:  0.10,
		InventoryPurchases: 0.20,
		Refunds:            0.10,
	}
	if income := canonicalIncome(snapshot); income != 0.30 {
		t.Fatalf("income=%v, want exact two-decimal 0.30", income)
	}
	if outflows := canonicalOutflows(snapshot); outflows != 0.40 {
		t.Fatalf("outflows=%v, want exact two-decimal 0.40", outflows)
	}
	if result := canonicalPeriodResult(snapshot); result != -0.10 {
		t.Fatalf("result=%v, want exact two-decimal -0.10", result)
	}
	kpi := newKPI(0.10+0.20, 0.10)
	if kpi.Current != 0.30 || kpi.Previous != 0.10 || kpi.Delta != 0.20 || kpi.DeltaPct == nil || *kpi.DeltaPct != 200 {
		t.Fatalf("cent-safe KPI=%+v", kpi)
	}
}
