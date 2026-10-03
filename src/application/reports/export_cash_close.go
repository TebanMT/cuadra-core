package reports

import (
	"fmt"
	"sort"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	"github.com/xuri/excelize/v2"
)

// El export del corte separa los cobros de referencia del control físico. No
// deriva un "neto" mezclando transferencias/tarjetas con salidas del cajón.
func renderCashClosePDF(gym *gymDomain.Gym, r *CashCloseReportOutput) []byte {
	pdf := newDoc()
	pdfHeader(pdf, gym, "Corte de caja", r.Date, r.Date)

	pdf.SetFont("Helvetica", "B", 10)
	pdf.CellFormat(0, 6, asciiSafe("Cobros registrados (todos los métodos)"), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 8)
	pdf.CellFormat(0, 5, asciiSafe("Referencia operativa; transferencia y tarjeta no forman el efectivo esperado."), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "B", 9)
	pdf.CellFormat(80, 6, asciiSafe("Concepto"), "1", 0, "L", false, 0, "")
	pdf.CellFormat(45, 6, asciiSafe("Monto"), "1", 1, "R", false, 0, "")
	pdf.SetFont("Helvetica", "", 9)
	write := func(label string, value float64) {
		pdf.CellFormat(80, 5, asciiSafe(label), "1", 0, "L", false, 0, "")
		pdf.CellFormat(45, 5, fmt.Sprintf("$%.2f", value), "1", 1, "R", false, 0, "")
	}
	write("Cobros registrados", r.Totals.GrandTotal)
	write("Devoluciones", -r.Totals.RefundTotal)
	for _, method := range []string{"cash", "transfer", "card"} {
		write("Cobros · "+cashCloseMethodLabel(method), r.Totals.ByMethod[method])
	}

	if r.Session != nil {
		pdf.Ln(3)
		pdf.SetFont("Helvetica", "B", 10)
		pdf.CellFormat(0, 6, asciiSafe("Control físico de caja"), "", 1, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 9)
		for _, metric := range cashClosePhysicalMetrics(r.Session) {
			pdf.CellFormat(80, 5, asciiSafe(metric.Label), "1", 0, "L", false, 0, "")
			if metric.Value == nil {
				pdf.CellFormat(45, 5, asciiSafe(metric.Empty), "1", 1, "R", false, 0, "")
			} else {
				pdf.CellFormat(45, 5, fmt.Sprintf("$%.2f", *metric.Value), "1", 1, "R", false, 0, "")
			}
		}
	}

	if len(r.Expenses) > 0 {
		pdf.Ln(3)
		pdf.SetFont("Helvetica", "B", 10)
		pdf.CellFormat(0, 6, asciiSafe("Gastos pagados desde caja"), "", 1, "L", false, 0, "")
		pdf.SetFont("Helvetica", "B", 8)
		widths := []float64{48, 70, 30, 30}
		for i, h := range []string{"Categoría", "Descripción", "Método", "Monto"} {
			pdf.CellFormat(widths[i], 6, asciiSafe(h), "1", 0, "C", false, 0, "")
		}
		pdf.Ln(-1)
		pdf.SetFont("Helvetica", "", 8)
		for _, e := range r.Expenses {
			desc := ""
			if e.Description != nil {
				desc = *e.Description
			}
			pdf.CellFormat(widths[0], 5, asciiSafe(truncate(labelCategory(e.Category), 30)), "1", 0, "L", false, 0, "")
			pdf.CellFormat(widths[1], 5, asciiSafe(truncate(desc, 45)), "1", 0, "L", false, 0, "")
			pdf.CellFormat(widths[2], 5, asciiSafe(cashCloseMethodLabel(e.PaymentMethod)), "1", 0, "L", false, 0, "")
			pdf.CellFormat(widths[3], 5, fmt.Sprintf("$%.2f", e.Amount), "1", 0, "R", false, 0, "")
			pdf.Ln(-1)
		}
	}
	return bytesOf(pdf)
}

func renderCashCloseXLSX(r *CashCloseReportOutput) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	bold, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return nil, err
	}
	row := 1
	put := func(values ...any) error {
		if err := writeRow(f, row, values); err != nil {
			return err
		}
		row++
		return nil
	}
	if err := put("Corte de caja", r.Date.Format("2006-01-02")); err != nil {
		return nil, err
	}
	_ = f.SetCellStyle(sheetName, "A1", "B1", bold)
	if err := put("Alcance", "Efectivo: caja seleccionada. Transferencia y tarjeta: referencia global del gimnasio; no cambian el efectivo esperado."); err != nil {
		return nil, err
	}
	if err := put("Concepto", "Monto"); err != nil {
		return nil, err
	}
	_ = f.SetCellStyle(sheetName, "A3", "B3", bold)
	for _, line := range [][]any{
		{"Cobros registrados", r.Totals.GrandTotal},
		{"Devoluciones", -r.Totals.RefundTotal},
	} {
		if err := put(line...); err != nil {
			return nil, err
		}
	}
	for _, method := range []string{"cash", "transfer", "card"} {
		if err := put("Cobros · "+cashCloseMethodLabel(method), r.Totals.ByMethod[method]); err != nil {
			return nil, err
		}
	}

	if r.Session != nil {
		row++
		if err := put("Control físico de caja"); err != nil {
			return nil, err
		}
		_ = f.SetCellStyle(sheetName, fmt.Sprintf("A%d", row-1), fmt.Sprintf("A%d", row-1), bold)
		for _, metric := range cashClosePhysicalMetrics(r.Session) {
			value := any(metric.Empty)
			if metric.Value != nil {
				value = *metric.Value
			}
			if err := put(metric.Label, value); err != nil {
				return nil, err
			}
		}
	}

	row++
	if err := put("Gastos pagados desde caja"); err != nil {
		return nil, err
	}
	_ = f.SetCellStyle(sheetName, fmt.Sprintf("A%d", row-1), fmt.Sprintf("A%d", row-1), bold)
	if err := put("Categoría", "Descripción", "Método", "Monto"); err != nil {
		return nil, err
	}
	_ = f.SetCellStyle(sheetName, fmt.Sprintf("A%d", row-1), fmt.Sprintf("D%d", row-1), bold)
	for _, e := range r.Expenses {
		desc := ""
		if e.Description != nil {
			desc = *e.Description
		}
		if err := put(labelCategory(e.Category), desc, cashCloseMethodLabel(e.PaymentMethod), e.Amount); err != nil {
			return nil, err
		}
	}

	// Desglose por concepto, orden estable para facilitar conciliación.
	row++
	if err := put("Cobros por concepto"); err != nil {
		return nil, err
	}
	_ = f.SetCellStyle(sheetName, fmt.Sprintf("A%d", row-1), fmt.Sprintf("A%d", row-1), bold)
	if err := put("Concepto", "Operaciones", "Monto"); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(r.Totals.ByConcept))
	for k := range r.Totals.ByConcept {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := r.Totals.ByConcept[k]
		if err := put(k, v.Count, v.Total); err != nil {
			return nil, err
		}
	}
	return toBytes(f)
}

type cashClosePhysicalMetric struct {
	Label string
	Value *float64
	Empty string
}

// cashClosePhysicalMetrics is intentionally sourced only from one cash
// session. Payment totals by transfer/card never participate in these rows.
func cashClosePhysicalMetrics(session *CashSessionView) []cashClosePhysicalMetric {
	if session == nil {
		return nil
	}
	value := func(v float64) *float64 {
		x := v
		return &x
	}
	opening := value(session.OpeningCash)
	if !session.OpeningCashKnown {
		opening = nil
	}
	metrics := []cashClosePhysicalMetric{
		{Label: "Efectivo inicial", Value: opening, Empty: "No confirmado"},
		{Label: "Movimiento neto de efectivo", Value: value(session.ActivityCash)},
		{Label: "Efectivo esperado", Value: value(session.ExpectedCash)},
		{Label: "Efectivo contado", Value: session.CountedCash, Empty: "Pendiente"},
	}
	difference := session.Difference
	if session.IsStale || session.AdjustedAfterWithdrawal {
		difference = nil
	}
	metrics = append(metrics, cashClosePhysicalMetric{Label: "Diferencia", Value: difference, Empty: "Pendiente"})
	if session.WithdrawnCash != nil {
		metrics = append(metrics, cashClosePhysicalMetric{Label: "Efectivo retirado", Value: session.WithdrawnCash})
	}
	if session.CashLeft != nil {
		metrics = append(metrics, cashClosePhysicalMetric{Label: "Efectivo dejado en caja", Value: session.CashLeft})
	}
	if session.RequiresNewSession || session.UncoveredCashActivity != 0 {
		metrics = append(metrics, cashClosePhysicalMetric{Label: "Actividad pendiente de nuevo corte", Value: value(session.UncoveredCashActivity)})
	}
	return metrics
}

func cashCloseMethodLabel(method string) string {
	switch method {
	case "cash":
		return "Efectivo"
	case "transfer":
		return "Transferencia"
	case "card":
		return "Tarjeta"
	default:
		return method
	}
}
