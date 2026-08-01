// PDF + XLSX renderers para el "period summary" — UC-036 export del range
// report completo. Vive en su propio archivo para no inflar export_pdf.go /
// export_xlsx.go, ya bastante poblados. Las dos funciones expuestas son
// renderPeriodSummaryPDF y renderPeriodSummaryXLSX; el resto son helpers
// privados de armar bloques de KPIs / breakdowns / tablas.
package reports

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	"github.com/xuri/excelize/v2"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
)

// expenseCategoryLabel — labels human-friendly. Si el enum crece, sólo hay
// que agregar acá; las llaves desconocidas caen a la propia key.
var expenseCategoryLabel = map[string]string{
	"renta":              "Renta",
	"servicios":          "Servicios",
	"mantenimiento":      "Mantenimiento",
	"sueldos":            "Sueldos",
	"marketing":          "Marketing",
	"mercaderia_externa": "Mercadería externa",
	"otros":              "Otros",
}

func labelCategory(k string) string {
	if v, ok := expenseCategoryLabel[k]; ok {
		return v
	}
	return k
}

// ---------------------------------------------------------------------------
// PDF
// ---------------------------------------------------------------------------

func renderPeriodSummaryPDF(gym *gymDomain.Gym, r *RangeReportOutput, from, to time.Time) []byte {
	pdf := newDoc()
	pdfHeader(pdf, gym, "Resumen del período", from, to)

	// --- Stock crítico arriba (snapshot, no varía con período).
	pdf.SetFont("Helvetica", "B", 10)
	pdf.CellFormat(0, 6,
		asciiSafe(fmt.Sprintf("Stock crítico (snapshot): %d sin stock · %d bajo mínimo",
			r.CriticalStock.OutCount, r.CriticalStock.LowCount)),
		"", 1, "L", false, 0, "")
	pdf.Ln(2)

	// --- Tabla de KPIs con deltas vs período anterior.
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(0, 6, asciiSafe("Indicadores del período"), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "B", 9)
	kpiHeaders := []string{"Indicador", "Actual", "Anterior", "Cambio"}
	kpiWidths := []float64{70, 35, 35, 35}
	for i, h := range kpiHeaders {
		pdf.CellFormat(kpiWidths[i], 6, asciiSafe(h), "1", 0, "C", false, 0, "")
	}
	pdf.Ln(-1)
	pdf.SetFont("Helvetica", "", 8)

	writeKPI := func(label string, k KPI, money bool) {
		pdf.CellFormat(kpiWidths[0], 5, asciiSafe(label), "1", 0, "L", false, 0, "")
		pdf.CellFormat(kpiWidths[1], 5, fmtKPIValue(k.Current, money), "1", 0, "R", false, 0, "")
		pdf.CellFormat(kpiWidths[2], 5, fmtKPIValue(k.Previous, money), "1", 0, "R", false, 0, "")
		pdf.CellFormat(kpiWidths[3], 5, asciiSafe(fmtKPIDelta(k)), "1", 0, "R", false, 0, "")
		pdf.Ln(-1)
	}
	writeKPI("Utilidad (Ingresos - egresos - devoluciones)", r.Totals.Net, true)
	writeKPI("Ingresos", r.Totals.Income, true)
	writeKPI("Egresos por mercancía", r.Totals.InventoryCost, true)
	writeKPI("Otros gastos", r.Totals.ExpensesGeneral, true)
	writeKPI("Devoluciones", r.Totals.Refunds, true)
	writeKPI("Socios nuevos", r.Totals.NewMembers, false)
	writeKPI("Check-ins", r.Totals.Checkins, false)
	pdf.Ln(3)

	// --- Ingresos vs Egresos por día (tabla; las gráficas no se rederean en PDF).
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(0, 6, asciiSafe("Ingresos vs Egresos por día"), "", 1, "L", false, 0, "")
	mergedDay := mergeDailySeries(r.IncomeByDay, r.ExpensesByDay)
	pdf.SetFont("Helvetica", "B", 9)
	dayHeaders := []string{"Día", "Ingresos", "Egresos", "Neto"}
	dayWidths := []float64{40, 38, 38, 38}
	for i, h := range dayHeaders {
		pdf.CellFormat(dayWidths[i], 6, asciiSafe(h), "1", 0, "C", false, 0, "")
	}
	pdf.Ln(-1)
	pdf.SetFont("Helvetica", "", 8)
	if len(mergedDay) == 0 {
		pdf.SetFont("Helvetica", "I", 8)
		pdf.CellFormat(0, 5, asciiSafe("Sin movimientos en el período."), "", 1, "L", false, 0, "")
	}
	for _, d := range mergedDay {
		pdf.CellFormat(dayWidths[0], 5, d.Date.Format("2006-01-02"), "1", 0, "C", false, 0, "")
		pdf.CellFormat(dayWidths[1], 5, fmt.Sprintf("$%.2f", d.Income), "1", 0, "R", false, 0, "")
		pdf.CellFormat(dayWidths[2], 5, fmt.Sprintf("$%.2f", d.Expenses), "1", 0, "R", false, 0, "")
		pdf.CellFormat(dayWidths[3], 5, fmt.Sprintf("$%.2f", d.Income-d.Expenses), "1", 0, "R", false, 0, "")
		pdf.Ln(-1)
	}
	pdf.Ln(3)

	// --- Gastos por categoría.
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(0, 6, asciiSafe("Por categoría"), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "B", 9)
	catWidths := []float64{80, 50}
	pdf.CellFormat(catWidths[0], 6, asciiSafe("Categoría"), "1", 0, "C", false, 0, "")
	pdf.CellFormat(catWidths[1], 6, asciiSafe("Total"), "1", 0, "C", false, 0, "")
	pdf.Ln(-1)
	pdf.SetFont("Helvetica", "", 8)
	if len(r.ExpensesByCategory) == 0 {
		pdf.SetFont("Helvetica", "I", 8)
		pdf.CellFormat(0, 5, asciiSafe("Sin gastos clasificados."), "", 1, "L", false, 0, "")
	}
	for _, e := range sortCategoryDesc(r.ExpensesByCategory) {
		pdf.CellFormat(catWidths[0], 5, asciiSafe(labelCategory(e.Key)), "1", 0, "L", false, 0, "")
		pdf.CellFormat(catWidths[1], 5, fmt.Sprintf("$%.2f", e.Value), "1", 0, "R", false, 0, "")
		pdf.Ln(-1)
	}
	pdf.Ln(3)

	// --- Top productos.
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(0, 6, asciiSafe("Top productos"), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "B", 9)
	tpWidths := []float64{80, 30, 40}
	pdf.CellFormat(tpWidths[0], 6, asciiSafe("Producto"), "1", 0, "C", false, 0, "")
	pdf.CellFormat(tpWidths[1], 6, asciiSafe("Unidades"), "1", 0, "C", false, 0, "")
	pdf.CellFormat(tpWidths[2], 6, asciiSafe("Revenue"), "1", 0, "C", false, 0, "")
	pdf.Ln(-1)
	pdf.SetFont("Helvetica", "", 8)
	if len(r.TopProducts) == 0 {
		pdf.SetFont("Helvetica", "I", 8)
		pdf.CellFormat(0, 5, asciiSafe("Sin ventas de productos en el período."), "", 1, "L", false, 0, "")
	}
	for _, p := range r.TopProducts {
		pdf.CellFormat(tpWidths[0], 5, asciiSafe(truncate(p.ProductName, 60)), "1", 0, "L", false, 0, "")
		pdf.CellFormat(tpWidths[1], 5, fmt.Sprintf("%d", p.Quantity), "1", 0, "R", false, 0, "")
		pdf.CellFormat(tpWidths[2], 5, fmt.Sprintf("$%.2f", p.Revenue), "1", 0, "R", false, 0, "")
		pdf.Ln(-1)
	}
	pdf.Ln(3)

	// --- Top socios.
	if len(r.TopMembers) > 0 {
		pdf.SetFont("Helvetica", "B", 11)
		pdf.CellFormat(0, 6, asciiSafe("Top socios pagadores"), "", 1, "L", false, 0, "")
		pdf.SetFont("Helvetica", "B", 9)
		tmW := []float64{80, 25, 40}
		pdf.CellFormat(tmW[0], 6, asciiSafe("Socio"), "1", 0, "C", false, 0, "")
		pdf.CellFormat(tmW[1], 6, asciiSafe("# Pagos"), "1", 0, "C", false, 0, "")
		pdf.CellFormat(tmW[2], 6, asciiSafe("Total"), "1", 0, "C", false, 0, "")
		pdf.Ln(-1)
		pdf.SetFont("Helvetica", "", 8)
		for _, m := range r.TopMembers {
			pdf.CellFormat(tmW[0], 5, asciiSafe(truncate(m.FullName, 60)), "1", 0, "L", false, 0, "")
			pdf.CellFormat(tmW[1], 5, fmt.Sprintf("%d", m.PaymentsCount), "1", 0, "R", false, 0, "")
			pdf.CellFormat(tmW[2], 5, fmt.Sprintf("$%.2f", m.TotalPaid), "1", 0, "R", false, 0, "")
			pdf.Ln(-1)
		}
		pdf.Ln(3)
	}

	return bytesOf(pdf)
}

// ---------------------------------------------------------------------------
// XLSX
// ---------------------------------------------------------------------------

func renderPeriodSummaryXLSX(r *RangeReportOutput, from, to time.Time) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	big, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 14}})

	rowIdx := 1
	header := func(text string, style int) {
		c, _ := excelize.CoordinatesToCellName(1, rowIdx)
		_ = f.SetCellValue(sheetName, c, text)
		_ = f.SetCellStyle(sheetName, c, c, style)
		rowIdx++
	}
	colsHeader := func(headers []string) {
		for i, h := range headers {
			c, _ := excelize.CoordinatesToCellName(i+1, rowIdx)
			_ = f.SetCellValue(sheetName, c, h)
			_ = f.SetCellStyle(sheetName, c, c, bold)
		}
		rowIdx++
	}
	row := func(values []any) {
		if err := writeRow(f, rowIdx, values); err == nil {
			rowIdx++
		}
	}

	header(fmt.Sprintf("Resumen del período (%s – %s)",
		from.Format("2006-01-02"), to.Format("2006-01-02")), big)
	header(fmt.Sprintf("Stock crítico: %d sin stock · %d bajo mínimo",
		r.CriticalStock.OutCount, r.CriticalStock.LowCount), bold)
	rowIdx++

	// KPI table — col1 indicador, col2 actual, col3 anterior, col4 delta.
	header("Indicadores", bold)
	colsHeader([]string{"Indicador", "Actual", "Anterior", "Cambio"})
	row([]any{"Utilidad (Ingresos - egresos - devoluciones)", r.Totals.Net.Current, r.Totals.Net.Previous, fmtKPIDelta(r.Totals.Net)})
	row([]any{"Ingresos", r.Totals.Income.Current, r.Totals.Income.Previous, fmtKPIDelta(r.Totals.Income)})
	row([]any{"Egresos por mercancía", r.Totals.InventoryCost.Current, r.Totals.InventoryCost.Previous, fmtKPIDelta(r.Totals.InventoryCost)})
	row([]any{"Otros gastos", r.Totals.ExpensesGeneral.Current, r.Totals.ExpensesGeneral.Previous, fmtKPIDelta(r.Totals.ExpensesGeneral)})
	row([]any{"Devoluciones", r.Totals.Refunds.Current, r.Totals.Refunds.Previous, fmtKPIDelta(r.Totals.Refunds)})
	row([]any{"Socios nuevos", r.Totals.NewMembers.Current, r.Totals.NewMembers.Previous, fmtKPIDelta(r.Totals.NewMembers)})
	row([]any{"Check-ins", r.Totals.Checkins.Current, r.Totals.Checkins.Previous, fmtKPIDelta(r.Totals.Checkins)})
	rowIdx++

	// Daily series.
	header("Ingresos vs Egresos por día", bold)
	colsHeader([]string{"Día", "Ingresos", "Egresos", "Neto"})
	for _, d := range mergeDailySeries(r.IncomeByDay, r.ExpensesByDay) {
		row([]any{d.Date.Format("2006-01-02"), d.Income, d.Expenses, d.Income - d.Expenses})
	}
	rowIdx++

	header("Gastos por categoría", bold)
	colsHeader([]string{"Categoría", "Total"})
	for _, e := range sortCategoryDesc(r.ExpensesByCategory) {
		row([]any{labelCategory(e.Key), e.Value})
	}
	rowIdx++

	header("Top productos", bold)
	colsHeader([]string{"Producto", "Unidades", "Revenue"})
	for _, p := range r.TopProducts {
		row([]any{p.ProductName, p.Quantity, p.Revenue})
	}
	rowIdx++

	header("Top socios", bold)
	colsHeader([]string{"Socio", "# Pagos", "Total"})
	for _, m := range r.TopMembers {
		row([]any{m.FullName, m.PaymentsCount, m.TotalPaid})
	}
	rowIdx++

	header("Gastos del período", bold)
	colsHeader([]string{"Fecha", "Categoría", "Descripción", "Método", "Monto"})
	for _, e := range r.Expenses {
		desc := ""
		if e.Description != nil {
			desc = *e.Description
		}
		row([]any{e.ExpenseDate.Format("2006-01-02"), labelCategory(e.Category), desc, e.PaymentMethod, e.Amount})
	}
	rowIdx++

	header("Compras de inventario", bold)
	colsHeader([]string{"Fecha", "Producto", "Unidades", "Costo unit.", "Costo total"})
	for _, ic := range r.InventoryCosts {
		row([]any{ic.OccurredAt.Format("2006-01-02"), ic.ProductName, ic.Delta, ic.CostUnit, ic.CostTotal})
	}

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// mergedDayRow — fila del bloque "Ingresos vs Egresos por día".
type mergedDayRow struct {
	Date     time.Time
	Income   float64
	Expenses float64
}

func mergeDailySeries(income []DailyIncome, expenses []DailyAmount) []mergedDayRow {
	bucket := map[string]*mergedDayRow{}
	for _, r := range income {
		key := r.Date.Format("2006-01-02")
		cur, ok := bucket[key]
		if !ok {
			cur = &mergedDayRow{Date: r.Date}
			bucket[key] = cur
		}
		cur.Income += r.Total
	}
	for _, r := range expenses {
		key := r.Date.Format("2006-01-02")
		cur, ok := bucket[key]
		if !ok {
			cur = &mergedDayRow{Date: r.Date}
			bucket[key] = cur
		}
		cur.Expenses += r.Total
	}
	out := make([]mergedDayRow, 0, len(bucket))
	for _, v := range bucket {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out
}

// categoryEntry — par categoría/total para tablas ordenadas. La definimos
// como struct vs un sort.Sort de tuplas para que el código que llama lea más
// limpio.
type categoryEntry struct {
	Key   string
	Value float64
}

func sortCategoryDesc(m map[string]float64) []categoryEntry {
	out := make([]categoryEntry, 0, len(m))
	for k, v := range m {
		if v > 0 {
			out = append(out, categoryEntry{Key: k, Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out
}

func fmtKPIValue(v float64, money bool) string {
	if money {
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("%.0f", v)
}

// fmtKPIDelta — prefer % when previous != 0, otherwise absolute. "—" when
// neither side gives signal.
func fmtKPIDelta(k KPI) string {
	if k.DeltaPct != nil {
		sign := ""
		if *k.DeltaPct > 0 {
			sign = "+"
		}
		return fmt.Sprintf("%s%.0f%%", sign, *k.DeltaPct)
	}
	if k.Current == 0 && k.Previous == 0 {
		return "—"
	}
	sign := ""
	if k.Delta > 0 {
		sign = "+"
	}
	return fmt.Sprintf("%s%.2f", sign, k.Delta)
}
