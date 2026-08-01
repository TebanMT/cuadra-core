package interfaces

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
)

// Las series diarias viajan como string "YYYY-MM-DD" — NUNCA como time.Time
// crudo. Marshalear el time.Time emitía "2026-07-31T00:00:00Z" y el FE, al
// formatear ese instante en hora local (CDMX = UTC-6), pintaba cada punto un
// día antes: la gráfica de ganancias salía "atrasada" un día. Este test fija
// el contrato del wire para que el bug no regrese con un refactor del DTO.
func TestDailySeriesWireEmitsDateOnlyStrings(t *testing.T) {
	day := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)

	income := dailyIncomeToWire([]reportsApp.DailyIncome{{Date: day, Total: 4200}})
	amounts := dailyAmountToWire([]reportsApp.DailyAmount{{Date: day, Total: 150}})
	counts := dailyCountToWire([]reportsApp.DailyCount{{Date: day, Count: 12}})

	for name, got := range map[string]string{
		"income": income[0].Date,
		"amount": amounts[0].Date,
		"count":  counts[0].Date,
	} {
		if got != "2026-07-31" {
			t.Errorf("%s series date = %q, want %q", name, got, "2026-07-31")
		}
	}

	// El JSON final no debe traer rastro de instante (T / Z) en date.
	raw, err := json.Marshal(income)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "T00:00:00") || strings.Contains(string(raw), "Z") {
		t.Errorf("wire JSON leaks an instant, want date-only: %s", raw)
	}

	// Slices nil → arrays vacíos (contrato existente del wire).
	if out := dailyIncomeToWire(nil); out == nil || len(out) != 0 {
		t.Errorf("nil income series should marshal as empty array")
	}
}
