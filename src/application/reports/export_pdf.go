// PDF renderers for UC-036. Uses jung-kurt/gofpdf for branded layouts; one
// page per report with header (gym name + range) + a simple table.
package reports

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/jung-kurt/gofpdf"
	"golang.org/x/text/encoding/charmap"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
)

// pdfHeader writes the gym branding banner used by every export. DA-36.2
// asks for "logo + nombre + datos fiscales si capturados" — we omit the logo
// in MVP (no upload UX yet) and render gym name + city + WhatsApp + RFC.
func pdfHeader(pdf *gofpdf.Fpdf, gym *gymDomain.Gym, title string, from, to time.Time) {
	pdf.SetFont("Helvetica", "B", 16)
	pdf.CellFormat(0, 8, asciiSafe(deref(gym.Name, "Tinta")), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 9)
	if gym.City != nil {
		pdf.CellFormat(0, 5, asciiSafe(*gym.City), "", 1, "L", false, 0, "")
	}
	if gym.WhatsApp != nil {
		pdf.CellFormat(0, 5, asciiSafe("WhatsApp: "+*gym.WhatsApp), "", 1, "L", false, 0, "")
	}
	if gym.RFC != nil {
		pdf.CellFormat(0, 5, asciiSafe("RFC: "+*gym.RFC), "", 1, "L", false, 0, "")
	}
	pdf.Ln(2)
	pdf.SetFont("Helvetica", "B", 13)
	pdf.CellFormat(0, 8, asciiSafe(title), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 9)
	pdf.CellFormat(0, 5,
		asciiSafe(fmt.Sprintf("Periodo: %s a %s", from.Format("2006-01-02"), to.Format("2006-01-02"))),
		"", 1, "L", false, 0, "")
	pdf.CellFormat(0, 5, asciiSafe("Generado: "+time.Now().UTC().Format("2006-01-02 15:04 UTC")),
		"", 1, "L", false, 0, "")
	pdf.Ln(3)
}

func newDoc() *gofpdf.Fpdf {
	pdf := gofpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(12, 12, 12)
	pdf.SetAutoPageBreak(true, 12)
	pdf.AddPage()
	return pdf
}

func bytesOf(pdf *gofpdf.Fpdf) []byte {
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		// gofpdf only errors on disk writes; in-memory buf shouldn't fail.
		// Falling back to empty bytes lets the controller surface a 500.
		return nil
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Members listing
// ---------------------------------------------------------------------------

func renderMembersPDF(gym *gymDomain.Gym, rows []MemberExportRow) []byte {
	pdf := newDoc()
	pdfHeader(pdf, gym, "Listado de socios", time.Time{}, time.Time{})
	pdf.SetFont("Helvetica", "B", 9)
	headers := []string{"Folio", "Nombre", "Tel.", "Plan", "Vence", "Estado"}
	widths := []float64{20, 55, 25, 35, 22, 23}
	for i, h := range headers {
		pdf.CellFormat(widths[i], 6, asciiSafe(h), "1", 0, "C", false, 0, "")
	}
	pdf.Ln(-1)
	pdf.SetFont("Helvetica", "", 8)
	for _, r := range rows {
		pdf.CellFormat(widths[0], 5, asciiSafe(r.Folio), "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[1], 5, asciiSafe(truncate(r.FullName, 38)), "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[2], 5, asciiSafe(r.Phone), "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[3], 5, asciiSafe(truncate(deref(r.PlanName, ""), 24)), "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[4], 5, optDate(r.ExpiryDate), "1", 0, "C", false, 0, "")
		pdf.CellFormat(widths[5], 5, asciiSafe(r.Status), "1", 0, "C", false, 0, "")
		pdf.Ln(-1)
	}
	pdf.Ln(3)
	pdf.SetFont("Helvetica", "I", 8)
	pdf.CellFormat(0, 5, asciiSafe(fmt.Sprintf("Total: %d socios", len(rows))), "", 1, "L", false, 0, "")
	return bytesOf(pdf)
}

// ---------------------------------------------------------------------------
// Payments
// ---------------------------------------------------------------------------

func renderPaymentsPDF(gym *gymDomain.Gym, rows []PaymentExportRow, from, to time.Time) []byte {
	pdf := newDoc()
	pdfHeader(pdf, gym, "Reporte de pagos", from, to)
	pdf.SetFont("Helvetica", "B", 9)
	headers := []string{"Folio", "Fecha", "Socio", "Concepto", "Método", "Monto", "Saldo"}
	widths := []float64{22, 22, 50, 28, 22, 20, 20}
	for i, h := range headers {
		pdf.CellFormat(widths[i], 6, asciiSafe(h), "1", 0, "C", false, 0, "")
	}
	pdf.Ln(-1)
	pdf.SetFont("Helvetica", "", 8)
	var total float64
	for _, r := range rows {
		pdf.CellFormat(widths[0], 5, asciiSafe(r.Folio), "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[1], 5, r.PaymentDate.Format("2006-01-02"), "1", 0, "C", false, 0, "")
		pdf.CellFormat(widths[2], 5, asciiSafe(truncate(deref(r.MemberFullName, "—"), 32)), "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[3], 5, asciiSafe(r.Concept), "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[4], 5, asciiSafe(r.Method), "1", 0, "C", false, 0, "")
		pdf.CellFormat(widths[5], 5, fmt.Sprintf("$%.2f", r.Amount), "1", 0, "R", false, 0, "")
		pdf.CellFormat(widths[6], 5, fmt.Sprintf("$%.2f", r.BalancePending), "1", 0, "R", false, 0, "")
		pdf.Ln(-1)
		if r.Concept != "refund" {
			total += r.Amount
		} else {
			total -= r.Amount
		}
	}
	pdf.Ln(3)
	pdf.SetFont("Helvetica", "B", 10)
	pdf.CellFormat(0, 6, asciiSafe(fmt.Sprintf("Total neto: $%.2f", total)), "", 1, "R", false, 0, "")
	return bytesOf(pdf)
}

// ---------------------------------------------------------------------------
// Sales
// ---------------------------------------------------------------------------

func renderSalesPDF(gym *gymDomain.Gym, rows []SaleExportRow, from, to time.Time) []byte {
	pdf := newDoc()
	pdfHeader(pdf, gym, "Reporte de ventas", from, to)
	pdf.SetFont("Helvetica", "B", 9)
	headers := []string{"Folio pago", "Fecha", "Socio", "Subtotal", "Descuento", "Total", "Método"}
	widths := []float64{25, 25, 50, 22, 22, 22, 18}
	for i, h := range headers {
		pdf.CellFormat(widths[i], 6, asciiSafe(h), "1", 0, "C", false, 0, "")
	}
	pdf.Ln(-1)
	pdf.SetFont("Helvetica", "", 8)
	var total float64
	for _, r := range rows {
		pdf.CellFormat(widths[0], 5, asciiSafe(r.PaymentFolio), "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[1], 5, r.CreatedAt.Format("2006-01-02"), "1", 0, "C", false, 0, "")
		pdf.CellFormat(widths[2], 5, asciiSafe(truncate(deref(r.MemberName, "—"), 32)), "1", 0, "L", false, 0, "")
		pdf.CellFormat(widths[3], 5, fmt.Sprintf("$%.2f", r.Subtotal), "1", 0, "R", false, 0, "")
		pdf.CellFormat(widths[4], 5, fmt.Sprintf("$%.2f", r.Discount), "1", 0, "R", false, 0, "")
		pdf.CellFormat(widths[5], 5, fmt.Sprintf("$%.2f", r.Total), "1", 0, "R", false, 0, "")
		pdf.CellFormat(widths[6], 5, asciiSafe(r.Method), "1", 0, "C", false, 0, "")
		pdf.Ln(-1)
		total += r.Total
	}
	pdf.Ln(3)
	pdf.SetFont("Helvetica", "B", 10)
	pdf.CellFormat(0, 6, asciiSafe(fmt.Sprintf("Total: $%.2f", total)), "", 1, "R", false, 0, "")
	return bytesOf(pdf)
}

// ---------------------------------------------------------------------------
// Attention required
// ---------------------------------------------------------------------------

func renderAttentionRequiredPDF(gym *gymDomain.Gym, out *AttentionRequiredOutput) []byte {
	pdf := newDoc()
	pdfHeader(pdf, gym, "Atención inmediata", time.Time{}, time.Time{})
	section := func(title string, rows int, lines [][]string, headers []string, widths []float64) {
		pdf.SetFont("Helvetica", "B", 11)
		pdf.CellFormat(0, 6, asciiSafe(fmt.Sprintf("%s (%d)", title, rows)), "", 1, "L", false, 0, "")
		if rows == 0 {
			pdf.SetFont("Helvetica", "I", 9)
			pdf.CellFormat(0, 5, asciiSafe("—"), "", 1, "L", false, 0, "")
			pdf.Ln(2)
			return
		}
		pdf.SetFont("Helvetica", "B", 9)
		for i, h := range headers {
			pdf.CellFormat(widths[i], 6, asciiSafe(h), "1", 0, "C", false, 0, "")
		}
		pdf.Ln(-1)
		pdf.SetFont("Helvetica", "", 8)
		for _, line := range lines {
			for i, c := range line {
				pdf.CellFormat(widths[i], 5, asciiSafe(truncate(c, 60)), "1", 0, "L", false, 0, "")
			}
			pdf.Ln(-1)
		}
		pdf.Ln(2)
	}

	expRows := make([][]string, 0, len(out.ExpiringSoon))
	for _, r := range out.ExpiringSoon {
		expRows = append(expRows, []string{r.FullName, r.Phone, r.ExpiryDate.Format("2006-01-02"), fmt.Sprintf("%dd", r.DaysLeft)})
	}
	section("Vencen próximamente", len(out.ExpiringSoon), expRows,
		[]string{"Socio", "Tel.", "Vence", "Días"}, []float64{70, 30, 30, 25})

	recoverRows := make([][]string, 0, len(out.RecoverableExpired))
	for _, r := range out.RecoverableExpired {
		recoverRows = append(recoverRows, []string{r.FullName, r.Phone, r.ExpiryDate.Format("2006-01-02"), fmt.Sprintf("%dd", r.DaysOverdue)})
	}
	section("Vencidos recuperables", len(out.RecoverableExpired), recoverRows,
		[]string{"Socio", "Tel.", "Venció", "Atraso"}, []float64{70, 30, 30, 25})

	inactiveRows := make([][]string, 0, len(out.InactiveInvoluntary))
	for _, r := range out.InactiveInvoluntary {
		last := "—"
		if r.LastCheckinAt != nil {
			last = r.LastCheckinAt.Format("2006-01-02")
		}
		inactiveRows = append(inactiveRows, []string{r.FullName, r.Phone, last, fmt.Sprintf("%dd", r.DaysAbsent)})
	}
	section("Inactivos involuntarios", len(out.InactiveInvoluntary), inactiveRows,
		[]string{"Socio", "Tel.", "Último check-in", "Sin venir"}, []float64{70, 30, 35, 25})

	stockRows := make([][]string, 0, len(out.LowStock))
	for _, r := range out.LowStock {
		stockRows = append(stockRows, []string{r.Name, fmt.Sprintf("%d", r.Stock), fmt.Sprintf("%d", r.StockMinimum)})
	}
	section("Stock bajo", len(out.LowStock), stockRows,
		[]string{"Producto", "Stock", "Mínimo"}, []float64{100, 25, 25})

	balRows := make([][]string, 0, len(out.PendingBalances))
	for _, r := range out.PendingBalances {
		balRows = append(balRows, []string{r.FullName, r.Phone, fmt.Sprintf("$%.2f", r.BalancePending)})
	}
	section("Saldos pendientes", len(out.PendingBalances), balRows,
		[]string{"Socio", "Tel.", "Saldo"}, []float64{90, 30, 30})

	bdayRows := make([][]string, 0, len(out.BirthdaysToday))
	for _, r := range out.BirthdaysToday {
		bdayRows = append(bdayRows, []string{r.FullName, r.Phone})
	}
	section("Cumpleañeros del día", len(out.BirthdaysToday), bdayRows,
		[]string{"Socio", "Tel."}, []float64{120, 30})

	return bytesOf(pdf)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// asciiSafe converts a UTF-8 string into the cp1252 (WinAnsi) byte sequence
// that gofpdf's standard Helvetica font expects.
//
// gofpdf doesn't auto-translate UTF-8 → WinAnsi: pasarle la "á" directo
// emite los dos bytes UTF-8 (0xC3 0xA1) que el reader interpreta como
// "Ã¡". El fix canónico es codificar cada rune a su byte cp1252 (charmap
// .Windows1252.EncodeRune). Runes fuera del codepage (CJK, emoji) caen
// a "?" para no romper el layout.
//
// Common es-MX glyphs round-trip cleanly: á/é/í/ó/ú/ñ/¿/¡/Á/É/Í/Ó/Ú/Ñ
// (Latin-1 range) y el em-dash "—" (U+2014 → byte 0x97).
func asciiSafe(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	for _, r := range s {
		if b, ok := charmap.Windows1252.EncodeRune(r); ok {
			buf.WriteByte(b)
			continue
		}
		buf.WriteByte('?')
	}
	return buf.String()
}

func deref(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

func optDate(t *time.Time) string {
	if t == nil {
		// Pre-encoded em-dash so callers can drop the result into
		// CellFormat without an extra asciiSafe (some sites do, some
		// don't — keeping this self-contained avoids the mismatch).
		return asciiSafe("—")
	}
	return t.Format("2006-01-02")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
