package app

import (
	"strings"
	"testing"
	"time"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
)

func TestCashDiffSummaryReadsAfterTheTemplateDollarSign(t *testing.T) {
	name, drawer := "Ana", "Caja 2"
	note := "  no había\ncambio.  "
	day := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	got := cashDiffSummary(reportsApp.CashCloseClosedEvent{
		CloseDate: day, CountedByName: &name, DrawerName: &drawer, Reason: &note,
	}, -125050)
	want := "1,250.50 (faltante) · contó Ana · Caja 2 · 12 nov · Nota: no había cambio"
	if got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if surplus := cashDiffSummary(reportsApp.CashCloseClosedEvent{}, 5); surplus != "0.05 (sobrante)" {
		t.Fatalf("bare surplus = %q", surplus)
	}
	long := strings.Repeat("a", 200)
	clipped := cashDiffSummary(reportsApp.CashCloseClosedEvent{Reason: &long}, 100)
	if !strings.HasSuffix(clipped, "…") || len([]rune(clipped)) > 110 {
		t.Fatalf("long note must be clipped: %q", clipped)
	}
}

func TestFormatPesoCentsGroupsThousands(t *testing.T) {
	for cents, want := range map[int64]string{0: "0.00", 7: "0.07", 99900: "999.00", 100000: "1,000.00", 123456789: "1,234,567.89"} {
		if got := formatPesoCents(cents); got != want {
			t.Errorf("formatPesoCents(%d) = %q, want %q", cents, got, want)
		}
	}
}
