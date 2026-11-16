package reports

import (
	"fmt"
	"math"
	"strings"

	"github.com/jung-kurt/gofpdf"
	"github.com/xuri/excelize/v2"
)

// La sección "Caja del período" del resumen exportado responde lo mismo que
// la tarjeta de la app, con las mismas palabras: cuánto entró, cuánto se
// retiró, cuánto faltó o sobró y qué días no tienen corte. Los estados de
// cada corte y el último conteo van al final como detalle. PDF y Excel salen
// de los mismos renglones para no contar dos versiones distintas.

// cashExportLine es un renglón de la sección. Amount nil → Text ocupa la
// columna del monto ("Sin conteo"); Detail explica de dónde sale el número.
type cashExportLine struct {
	Label  string
	Amount *float64
	Text   string
	Detail string
}

// cashExportDifference es un corte (o una apertura) donde faltó o sobró.
type cashExportDifference struct {
	Date       string
	Moment     string
	Drawer     string
	Difference float64
	Detail     string
}

type periodCashExport struct {
	Status      string
	Answers     []cashExportLine
	Pending     []cashExportLine
	Differences []cashExportDifference
	Latest      []cashExportLine
	States      string
}

func exportPlural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func exportMoney(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}

func exportAmount(v float64) *float64 {
	return &v
}

func exportDifferenceResult(v float64) string {
	if math.Abs(v) < 0.005 {
		return "Cuadra"
	}
	if v < 0 {
		return "Faltan " + exportMoney(-v)
	}
	return "Sobran " + exportMoney(v)
}

func joinDetail(parts ...*string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != nil && strings.TrimSpace(*p) != "" {
			out = append(out, strings.TrimSpace(*p))
		}
	}
	return strings.Join(out, " · ")
}

func prefixed(prefix string, name *string) *string {
	if name == nil || strings.TrimSpace(*name) == "" {
		return nil
	}
	s := prefix + strings.TrimSpace(*name)
	return &s
}

func buildPeriodCashExport(c CashReconciliation) periodCashExport {
	out := periodCashExport{}
	stale := c.Status == "stale" || c.LatestNeedsRecount || c.StaleSessions > 0
	switch {
	case stale:
		out.Status = "Volver a contar"
	case c.Complete:
		out.Status = "Caja revisada"
	case c.RequiresAttention:
		out.Status = "Por completar"
	case c.ActiveSessions == 0:
		out.Status = "Sin cortes en el período"
	default:
		out.Status = "Sin pendientes"
	}

	activity := c.PeriodActivity
	in := cashExportLine{Label: "Entró a la caja", Amount: exportAmount(activity),
		Detail: "Cobros en efectivo, menos gastos y devoluciones pagados de la caja"}
	if activity < 0 {
		in = cashExportLine{Label: "Salió de la caja", Amount: exportAmount(-activity),
			Detail: "Se pagó más de la caja de lo que se cobró en efectivo"}
	}
	withdrawn := cashExportLine{Label: "Se retiró de la caja", Amount: exportAmount(c.PeriodWithdrawn),
		Detail: "Sin retiros registrados"}
	if c.WithdrawnSessions > 0 {
		withdrawn.Detail = "En " + exportPlural(c.WithdrawnSessions, "retiro", "retiros")
		if c.PeriodWithdrawn > math.Max(activity, 0) {
			// El retiro puede superar lo que entró porque incluye el efectivo inicial.
			withdrawn.Detail += " · incluye efectivo que ya estaba al abrir"
		}
	}
	out.Answers = append(out.Answers, in, withdrawn)

	counted := c.CountedCloses > 0 || c.ReconciledSessions > 0 || len(c.DifferenceSessions) > 0
	shortage := cashExportLine{Label: "Faltó en los cortes", Text: "Sin cortes contados"}
	surplus := cashExportLine{Label: "Sobró en los cortes", Text: "Sin cortes contados"}
	if counted {
		// Cada corte se cuenta aparte: un sobrante no cubre un faltante.
		shortage = cashExportLine{Label: shortage.Label, Amount: exportAmount(c.ShortageTotal),
			Detail: "Ningún corte con faltante"}
		if c.ShortageSessions > 0 {
			shortage.Detail = "En " + exportPlural(c.ShortageSessions, "corte", "cortes")
		}
		surplus = cashExportLine{Label: surplus.Label, Amount: exportAmount(c.SurplusTotal),
			Detail: "Ningún corte con sobrante"}
		if c.SurplusSessions > 0 {
			surplus.Detail = "En " + exportPlural(c.SurplusSessions, "corte", "cortes")
		}
	}
	out.Answers = append(out.Answers, shortage, surplus)
	if len(c.OpeningDifferences) > 0 {
		out.Answers = append(out.Answers,
			cashExportLine{Label: "Faltó al abrir la caja", Amount: exportAmount(c.OpeningShortageTotal),
				Detail: openingCountDetail(c.OpeningShortageSessions, "Ninguna apertura con faltante")},
			cashExportLine{Label: "Sobró al abrir la caja", Amount: exportAmount(c.OpeningSurplusTotal),
				Detail: openingCountDetail(c.OpeningSurplusSessions, "Ninguna apertura con sobrante")},
		)
	}

	// Lo primero que el dueño busca: qué días no tienen corte.
	if c.UncoveredActivityDays > 0 {
		out.Pending = append(out.Pending, cashExportLine{
			Label:  exportPlural(c.UncoveredActivityDays, "día", "días") + " sin corte terminado",
			Detail: strings.Join(c.UncoveredActivityDates, ", "),
		})
	} else if c.MissingActiveDays > 0 {
		out.Pending = append(out.Pending, cashExportLine{
			Label: exportPlural(c.MissingActiveDays, "día", "días") + " sin abrir la caja",
		})
	}
	pending := func(n int, text string) {
		if n > 0 {
			out.Pending = append(out.Pending, cashExportLine{Label: text})
		}
	}
	if c.OpenSessions == 1 {
		pending(1, "1 caja sigue abierta")
	} else {
		pending(c.OpenSessions, fmt.Sprintf("%d cajas siguen abiertas", c.OpenSessions))
	}
	pending(c.ClosedUnverifiedSessions,
		exportPlural(c.ClosedUnverifiedSessions, "corte se cerró", "cortes se cerraron")+" sin contar el efectivo")
	pending(c.UnknownOpeningSessions,
		"En "+exportPlural(c.UnknownOpeningSessions, "corte", "cortes")+" no se anotó con cuánto efectivo se empezó")
	pending(c.AdjustedAfterWithdrawalSessions,
		exportPlural(c.AdjustedAfterWithdrawalSessions, "corte cambió", "cortes cambiaron")+" después de retirar el efectivo")
	paid := "se pagaron"
	if c.LegacyCashSourceUnverifiedCount == 1 {
		paid = "se pagó"
	}
	pending(c.LegacyCashSourceUnverifiedCount,
		exportPlural(c.LegacyCashSourceUnverifiedCount, "gasto antiguo", "gastos antiguos")+
			": falta decir si "+paid+" con efectivo de la caja")
	pending(c.StaleSessions, "Hay que volver a contar "+exportPlural(c.StaleSessions, "corte", "cortes"))

	for _, d := range c.DifferenceSessions {
		moment := "Corte"
		if d.Sequence > 1 {
			moment = fmt.Sprintf("Corte %d", d.Sequence)
		}
		out.Differences = append(out.Differences, cashExportDifference{
			Date: d.Date, Moment: moment, Drawer: deref(d.DrawerName, ""), Difference: d.Difference,
			Detail: joinDetail(prefixed("Contó ", d.CountedByName), d.Reason),
		})
	}
	for _, o := range c.OpeningDifferences {
		out.Differences = append(out.Differences, cashExportDifference{
			Date: o.Date, Moment: "Al abrir", Drawer: deref(o.DrawerName, ""), Difference: o.Difference,
			Detail: joinDetail(prefixed("Abrió ", o.OpenedByName), o.Note),
		})
	}

	if c.LatestExpected != nil || c.LatestCounted != nil {
		// El último corte es un saldo puntual: no habla del período completo.
		expected := cashExportLine{Label: "Debía haber", Amount: c.LatestExpected, Text: "Sin calcular",
			Detail: "Efectivo inicial más entradas, menos salidas"}
		countedLine := cashExportLine{Label: "Se contó", Amount: c.LatestCounted, Text: "Sin conteo"}
		difference := cashExportLine{Label: "Diferencia del último corte", Amount: c.LatestDifference}
		switch {
		case c.LatestDifference != nil:
			difference.Detail = exportDifferenceResult(*c.LatestDifference)
		case stale:
			difference.Text = "Hubo movimientos después de contar"
		default:
			difference.Text = "Falta contar el efectivo"
		}
		out.Latest = []cashExportLine{expected, countedLine, difference}
	}

	states := []string{}
	add := func(n int, one, many string) {
		if n > 0 {
			states = append(states, exportPlural(n, one, many))
		}
	}
	add(c.ActiveSessions, "corte en el período", "cortes en el período")
	add(c.ReconciledSessions, "contado", "contados")
	add(c.WithdrawnSessions, "con retiro", "con retiro")
	add(c.OpenSessions, "abierto", "abiertos")
	add(c.ClosedUnverifiedSessions, "cerrado sin contar", "cerrados sin contar")
	add(c.ReviewedSessions, "cerrado por el dueño sin conteo", "cerrados por el dueño sin conteo")
	add(c.StaleSessions, "por volver a contar", "por volver a contar")
	out.States = strings.Join(states, " · ")
	return out
}

func openingCountDetail(n int, none string) string {
	if n == 0 {
		return none
	}
	return "En " + exportPlural(n, "apertura", "aperturas")
}

// ---------------------------------------------------------------------------
// PDF
// ---------------------------------------------------------------------------

func writePeriodCashPDF(pdf *gofpdf.Fpdf, c CashReconciliation) {
	section := buildPeriodCashExport(c)
	widths := []float64{62, 32, 96}
	subtitle := func(text string) {
		pdf.SetFont("Helvetica", "B", 9)
		pdf.CellFormat(0, 6, asciiSafe(text), "", 1, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 8)
	}
	line := func(l cashExportLine) {
		value := l.Text
		if l.Amount != nil {
			value = exportMoney(*l.Amount)
		}
		pdf.CellFormat(widths[0], 5, asciiSafe(l.Label), "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[1], 5, asciiSafe(value), "1", 0, "R", false, 0, "")
		pdf.CellFormat(widths[2], 5, asciiSafe(truncate(l.Detail, 70)), "1", 1, "L", false, 0, "")
	}

	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(0, 6, asciiSafe("Caja del período · "+section.Status), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 8)
	for _, l := range section.Answers {
		line(l)
	}
	if len(section.Pending) > 0 {
		pdf.Ln(1)
		subtitle("Falta completar la caja")
		for _, p := range section.Pending {
			text := p.Label
			if p.Detail != "" {
				text += ": " + p.Detail
			}
			pdf.MultiCell(0, 4.5, asciiSafe("- "+text), "", "L", false)
		}
	}
	if len(section.Differences) > 0 {
		pdf.Ln(1)
		subtitle("Faltantes y sobrantes, uno por uno")
		dw := []float64{22, 20, 32, 30, 86}
		pdf.SetFont("Helvetica", "B", 8)
		for i, h := range []string{"Fecha", "Momento", "Caja", "Resultado", "Quién y nota"} {
			pdf.CellFormat(dw[i], 5, asciiSafe(h), "1", 0, "C", false, 0, "")
		}
		pdf.Ln(-1)
		pdf.SetFont("Helvetica", "", 8)
		for _, d := range section.Differences {
			pdf.CellFormat(dw[0], 5, asciiSafe(d.Date), "1", 0, "L", false, 0, "")
			pdf.CellFormat(dw[1], 5, asciiSafe(d.Moment), "1", 0, "L", false, 0, "")
			pdf.CellFormat(dw[2], 5, asciiSafe(truncate(d.Drawer, 20)), "1", 0, "L", false, 0, "")
			pdf.CellFormat(dw[3], 5, asciiSafe(exportDifferenceResult(d.Difference)), "1", 0, "R", false, 0, "")
			pdf.CellFormat(dw[4], 5, asciiSafe(truncate(d.Detail, 60)), "1", 1, "L", false, 0, "")
		}
	}
	if len(section.Latest) > 0 || section.States != "" {
		pdf.Ln(1)
		subtitle("Detalle de los cortes")
		for _, l := range section.Latest {
			line(l)
		}
		if section.States != "" {
			pdf.MultiCell(0, 4.5, asciiSafe(section.States), "", "L", false)
		}
	}
	pdf.Ln(3)
}

// ---------------------------------------------------------------------------
// XLSX
// ---------------------------------------------------------------------------

// writePeriodCashXLSX escribe la sección desde rowIdx y devuelve la siguiente
// fila libre. Los montos quedan como números para que el dueño pueda sumarlos.
func writePeriodCashXLSX(f *excelize.File, rowIdx int, bold int, c CashReconciliation) int {
	section := buildPeriodCashExport(c)
	put := func(values []any, style bool) {
		if err := writeRow(f, rowIdx, values); err != nil {
			return
		}
		if style {
			first, _ := excelize.CoordinatesToCellName(1, rowIdx)
			last, _ := excelize.CoordinatesToCellName(len(values), rowIdx)
			_ = f.SetCellStyle(sheetName, first, last, bold)
		}
		rowIdx++
	}
	line := func(l cashExportLine) {
		var value any = l.Text
		if l.Amount != nil {
			value = *l.Amount
		}
		put([]any{l.Label, value, l.Detail}, false)
	}

	put([]any{"Caja del período", section.Status}, true)
	put([]any{"Concepto", "Monto", "Detalle"}, true)
	for _, l := range section.Answers {
		line(l)
	}
	if len(section.Pending) > 0 {
		rowIdx++
		put([]any{"Falta completar la caja"}, true)
		for _, p := range section.Pending {
			put([]any{p.Label, p.Detail}, false)
		}
	}
	if len(section.Differences) > 0 {
		rowIdx++
		put([]any{"Faltantes y sobrantes, uno por uno"}, true)
		put([]any{"Fecha", "Momento", "Caja", "Resultado", "Diferencia", "Quién y nota"}, true)
		for _, d := range section.Differences {
			put([]any{d.Date, d.Moment, d.Drawer, exportDifferenceResult(d.Difference), d.Difference, d.Detail}, false)
		}
	}
	if len(section.Latest) > 0 || section.States != "" {
		rowIdx++
		put([]any{"Detalle de los cortes"}, true)
		for _, l := range section.Latest {
			line(l)
		}
		if section.States != "" {
			put([]any{section.States}, false)
		}
	}
	return rowIdx
}
