// XLSX renderers for UC-036. Uses xuri/excelize/v2; one Sheet1 per export
// for now — owner can pivot later in Excel.
package reports

import (
	"bytes"
	"fmt"

	"github.com/xuri/excelize/v2"
)

const sheetName = "Sheet1"

// writeHeader sets row 1 cells (A..) to the given headers and bolds them.
func writeHeader(f *excelize.File, headers []string) error {
	style, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return err
	}
	for i, h := range headers {
		cell, err := excelize.CoordinatesToCellName(i+1, 1)
		if err != nil {
			return err
		}
		if err := f.SetCellValue(sheetName, cell, h); err != nil {
			return err
		}
		_ = f.SetCellStyle(sheetName, cell, cell, style)
	}
	return nil
}

func writeRow(f *excelize.File, rowIdx int, values []any) error {
	for i, v := range values {
		cell, err := excelize.CoordinatesToCellName(i+1, rowIdx)
		if err != nil {
			return err
		}
		if err := f.SetCellValue(sheetName, cell, v); err != nil {
			return err
		}
	}
	return nil
}

func toBytes(f *excelize.File) ([]byte, error) {
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func renderMembersXLSX(rows []MemberExportRow) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	if err := writeHeader(f, []string{
		"Folio", "Nombre", "Teléfono", "Email", "Estado", "Plan", "Inicio", "Vence", "Alta",
	}); err != nil {
		return nil, err
	}
	for i, r := range rows {
		if err := writeRow(f, i+2, []any{
			r.Folio, r.FullName, r.Phone, deref(r.Email, ""), r.Status,
			deref(r.PlanName, ""), optDate(r.StartDate), optDate(r.ExpiryDate),
			r.CreatedAt.Format("2006-01-02"),
		}); err != nil {
			return nil, err
		}
	}
	return toBytes(f)
}

func renderPaymentsXLSX(rows []PaymentExportRow) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	if err := writeHeader(f, []string{
		"Folio", "Fecha", "Socio", "Concepto", "Método", "Monto", "Descuento", "Saldo pendiente", "Operador",
	}); err != nil {
		return nil, err
	}
	for i, r := range rows {
		if err := writeRow(f, i+2, []any{
			r.Folio, r.PaymentDate.Format("2006-01-02"), deref(r.MemberFullName, ""),
			r.Concept, r.Method, r.Amount, r.Discount, r.BalancePending, deref(r.OperatorEmail, ""),
		}); err != nil {
			return nil, err
		}
	}
	return toBytes(f)
}

func renderSalesXLSX(rows []SaleExportRow) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	if err := writeHeader(f, []string{
		"Folio pago", "Fecha", "Socio", "Subtotal", "Descuento", "Total", "Método",
	}); err != nil {
		return nil, err
	}
	for i, r := range rows {
		if err := writeRow(f, i+2, []any{
			r.PaymentFolio, r.CreatedAt.Format("2006-01-02 15:04"), deref(r.MemberName, ""),
			r.Subtotal, r.Discount, r.Total, r.Method,
		}); err != nil {
			return nil, err
		}
	}
	return toBytes(f)
}

// renderAttentionRequiredXLSX writes one section per category, separated by
// blank rows + a bold section header. Single sheet keeps the file simple.
func renderAttentionRequiredXLSX(out *AttentionRequiredOutput) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	headerStyle, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 12}})
	if err != nil {
		return nil, err
	}
	rowIdx := 1
	writeSection := func(title string, headers []string, rows [][]any) error {
		cell, _ := excelize.CoordinatesToCellName(1, rowIdx)
		if err := f.SetCellValue(sheetName, cell, fmt.Sprintf("%s (%d)", title, len(rows))); err != nil {
			return err
		}
		_ = f.SetCellStyle(sheetName, cell, cell, headerStyle)
		rowIdx++
		colHeaderStyle, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
		for i, h := range headers {
			c, _ := excelize.CoordinatesToCellName(i+1, rowIdx)
			_ = f.SetCellValue(sheetName, c, h)
			_ = f.SetCellStyle(sheetName, c, c, colHeaderStyle)
		}
		rowIdx++
		for _, row := range rows {
			if err := writeRow(f, rowIdx, row); err != nil {
				return err
			}
			rowIdx++
		}
		rowIdx++
		return nil
	}

	// Each section. We could DRY this up further but the readability cost is
	// low — six sections, each with a different header schema.
	expR := make([][]any, len(out.ExpiringSoon))
	for i, r := range out.ExpiringSoon {
		expR[i] = []any{r.FullName, r.Phone, r.ExpiryDate.Format("2006-01-02"), r.DaysLeft}
	}
	if err := writeSection("Vencen próximamente", []string{"Socio", "Teléfono", "Vence", "Días"}, expR); err != nil {
		return nil, err
	}

	recR := make([][]any, len(out.RecoverableExpired))
	for i, r := range out.RecoverableExpired {
		recR[i] = []any{r.FullName, r.Phone, r.ExpiryDate.Format("2006-01-02"), r.DaysOverdue}
	}
	if err := writeSection("Vencidos recuperables", []string{"Socio", "Teléfono", "Venció", "Días vencido"}, recR); err != nil {
		return nil, err
	}

	inaR := make([][]any, len(out.InactiveInvoluntary))
	for i, r := range out.InactiveInvoluntary {
		last := ""
		if r.LastCheckinAt != nil {
			last = r.LastCheckinAt.Format("2006-01-02")
		}
		inaR[i] = []any{r.FullName, r.Phone, last, r.DaysAbsent}
	}
	if err := writeSection("Inactivos involuntarios", []string{"Socio", "Teléfono", "Último check-in", "Días sin venir"}, inaR); err != nil {
		return nil, err
	}

	stkR := make([][]any, len(out.LowStock))
	for i, r := range out.LowStock {
		stkR[i] = []any{r.Name, r.Stock, r.StockMinimum}
	}
	if err := writeSection("Stock bajo", []string{"Producto", "Stock", "Mínimo"}, stkR); err != nil {
		return nil, err
	}

	balR := make([][]any, len(out.PendingBalances))
	for i, r := range out.PendingBalances {
		balR[i] = []any{r.FullName, r.Phone, r.BalancePending}
	}
	if err := writeSection("Saldos pendientes", []string{"Socio", "Teléfono", "Saldo"}, balR); err != nil {
		return nil, err
	}

	bdR := make([][]any, len(out.BirthdaysToday))
	for i, r := range out.BirthdaysToday {
		bdR[i] = []any{r.FullName, r.Phone}
	}
	if err := writeSection("Cumpleañeros del día", []string{"Socio", "Teléfono"}, bdR); err != nil {
		return nil, err
	}

	return toBytes(f)
}
