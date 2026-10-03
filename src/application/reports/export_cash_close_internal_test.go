package reports

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
)

func TestCashCloseExportKeepsNonCashCollectionsOutOfPhysicalExpectation(t *testing.T) {
	counted, difference, withdrawn, left := 75.0, -5.0, 70.0, 5.0
	report := &CashCloseReportOutput{
		Date: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC),
		Totals: &billingRepo.CashCloseTotals{
			GrandTotal: 1500,
			ByMethod: map[string]float64{
				"cash": 100, "transfer": 900, "card": 500,
			},
			ByConcept: map[string]billingRepo.ConceptTotal{},
		},
		// This deliberately impossible old alias proves the export never uses it.
		NetTotal: 1470,
		Session: &CashSessionView{
			OpeningCash: 20, OpeningCashKnown: true,
			ActivityCash: 60, ExpectedCash: 80,
			CountedCash: &counted, Difference: &difference,
			WithdrawnCash: &withdrawn, CashLeft: &left,
		},
	}

	data, err := renderCashCloseXLSX(report)
	if err != nil {
		t.Fatalf("render cash close: %v", err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open cash close xlsx: %v", err)
	}
	defer f.Close()
	rows, err := f.GetRows(sheetName)
	if err != nil {
		t.Fatalf("get rows: %v", err)
	}

	values := map[string]string{}
	var flat strings.Builder
	for _, row := range rows {
		for _, cell := range row {
			flat.WriteString(cell)
			flat.WriteByte('\n')
		}
		if len(row) >= 2 {
			values[row[0]] = row[1]
		}
	}
	if strings.Contains(flat.String(), "Flujo neto") {
		t.Fatalf("cash close export still publishes mixed Flujo neto: %s", flat.String())
	}
	for label, want := range map[string]float64{
		"Movimiento neto de efectivo": 60,
		"Efectivo esperado":           80,
		"Efectivo contado":            75,
		"Diferencia":                  -5,
		"Efectivo retirado":           70,
	} {
		got, parseErr := strconv.ParseFloat(values[label], 64)
		if parseErr != nil || got != want {
			t.Errorf("%s = %q (%v), want %.2f", label, values[label], parseErr, want)
		}
	}
	if values["Cobros · Transferencia"] != "900" || values["Cobros · Tarjeta"] != "500" {
		t.Fatalf("non-cash collections should remain reference rows: %+v", values)
	}
	if !strings.Contains(values["Alcance"], "no cambian el efectivo esperado") {
		t.Fatalf("cash/digital scope must be explicit in export: %+v", values)
	}
}

func TestCashClosePhysicalMetricsSuppressesInvalidatedDifference(t *testing.T) {
	difference := 12.0
	metrics := cashClosePhysicalMetrics(&CashSessionView{
		OpeningCashKnown: true, Difference: &difference, IsStale: true,
	})
	for _, metric := range metrics {
		if metric.Label == "Diferencia" && metric.Value != nil {
			t.Fatalf("stale session leaked a certified difference: %+v", metric)
		}
	}
}
