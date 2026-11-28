package app

import (
	"bytes"
	"strings"

	"github.com/jung-kurt/gofpdf"
)

// PDF del comprobante con forma de ticket (80 mm de ancho, alto exacto al
// contenido). Es el que viaja por WhatsApp y el de "Descargar": en el
// celular se lee como un ticket real y, si se imprime, cabe en un rollo
// térmico. El ticket impreso desde recepción usa la MISMA ReceiptView
// (HTML en el cliente), así que ambos dicen exactamente lo mismo.
const (
	ticketWidthMM  = 80.0
	ticketMarginMM = 5.0
	ticketContent  = ticketWidthMM - 2*ticketMarginMM
)

func renderReceiptPDF(v ReceiptView, logoPNG []byte) ([]byte, error) {
	// Pasada 1: medir el alto real sobre una página muy alta. Pasada 2:
	// página del alto exacto. gofpdf no permite cambiar el alto de una
	// página ya abierta.
	probe := newTicketDoc(2000)
	drawReceipt(probe, v, logoPNG)
	if err := probe.Error(); err != nil {
		return nil, err
	}
	height := probe.GetY() + ticketMarginMM

	pdf := newTicketDoc(height)
	drawReceipt(pdf, v, logoPNG)
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func newTicketDoc(heightMM float64) *gofpdf.Fpdf {
	pdf := gofpdf.NewCustom(&gofpdf.InitType{
		UnitStr: "mm",
		Size:    gofpdf.SizeType{Wd: ticketWidthMM, Ht: heightMM},
	})
	pdf.SetMargins(ticketMarginMM, ticketMarginMM, ticketMarginMM)
	pdf.SetAutoPageBreak(false, 0)
	pdf.SetCreator("Tinta", true)
	pdf.AddPage()
	return pdf
}

func drawReceipt(pdf *gofpdf.Fpdf, v ReceiptView, logoPNG []byte) {
	tr := pdf.UnicodeTranslatorFromDescriptor("") // cp1252: á é ñ ¿ ×
	pdf.SetTitle(tr(v.Title+" "+v.Folio), false)
	pdf.SetTextColor(20, 20, 20)
	left := ticketMarginMM

	// ── Encabezado del gym ──────────────────────────────────────────────
	if len(logoPNG) > 0 {
		opts := gofpdf.ImageOptions{ImageType: "PNG", ReadDpi: false}
		info := pdf.RegisterImageOptionsReader("logo", opts, bytes.NewReader(logoPNG))
		if pdf.Ok() && info != nil && info.Width() > 0 {
			w, h := 34.0, 34.0*info.Height()/info.Width()
			if h > 18 {
				w, h = w*18/h, 18
			}
			pdf.ImageOptions("logo", (ticketWidthMM-w)/2, pdf.GetY(), w, h, false, opts, 0, "")
			pdf.SetY(pdf.GetY() + h + 2)
		} else {
			pdf.ClearError()
		}
	}
	pdf.SetFont("Helvetica", "B", 11)
	centered(pdf, tr, v.Gym.Name, 5)
	pdf.SetFont("Helvetica", "", 7.5)
	for _, s := range []string{v.Gym.RazonSocial, v.Gym.Address} {
		if s != "" {
			centered(pdf, tr, s, 3.6)
		}
	}
	if v.Gym.RFC != "" {
		centered(pdf, tr, "RFC: "+v.Gym.RFC, 3.6)
	}
	if v.Gym.WhatsApp != "" {
		centered(pdf, tr, "WhatsApp: "+v.Gym.WhatsApp, 3.6)
	}
	divider(pdf)

	// ── Título + folio + fecha ─────────────────────────────────────────
	pdf.SetFont("Helvetica", "B", 10)
	centered(pdf, tr, strings.ToUpper(v.Title), 5)
	pdf.Ln(1)
	pdf.SetFont("Helvetica", "", 8)
	row(pdf, tr, "Folio", v.Folio, "", false)
	date := v.Date
	if v.Time != "" {
		date += ", " + v.Time
	}
	row(pdf, tr, "Fecha", date, "", false)
	if v.RegisteredAt != "" {
		row(pdf, tr, "Registrado", v.RegisteredAt, "", false)
	}

	// ── Socio + vigencia ───────────────────────────────────────────────
	if v.Member != nil {
		divider(pdf)
		row(pdf, tr, "Socio", v.Member.FullName, "", true)
		if v.Member.MemberNumber != "" {
			row(pdf, tr, "No. de socio", v.Member.MemberNumber, "", true)
		}
	}
	if v.Membership != nil {
		pdf.Ln(1)
		top := pdf.GetY()
		pdf.SetY(top + 1.5)
		pdf.SetFont("Helvetica", "", 8)
		row(pdf, tr, "Plan", v.Membership.PlanName, "", true)
		row(pdf, tr, "Inicio", v.Membership.StartDate, "", false)
		if v.Membership.ExpiryDate != "" {
			pdf.SetFont("Helvetica", "B", 10)
			row(pdf, tr, "Vence", v.Membership.ExpiryDate, "B", true)
			pdf.SetFont("Helvetica", "", 8)
		}
		pdf.SetY(pdf.GetY() + 1)
		pdf.SetDrawColor(60, 60, 60)
		pdf.SetLineWidth(0.3)
		pdf.Rect(left-1, top, ticketContent+2, pdf.GetY()-top, "D")
		pdf.SetY(pdf.GetY() + 1)
	}
	divider(pdf)

	// ── Detalle y totales ──────────────────────────────────────────────
	pdf.SetFont("Helvetica", "B", 8)
	row(pdf, tr, "Concepto", "Importe", "B", true)
	pdf.SetFont("Helvetica", "", 8)
	for _, l := range v.Lines {
		row(pdf, tr, l.Label, l.Value, "", false)
	}
	pdf.Ln(1)
	for _, t := range v.Totals {
		if t.Emphasis {
			pdf.SetFont("Helvetica", "B", 11)
			row(pdf, tr, t.Label, t.Value, "B", true)
			pdf.SetFont("Helvetica", "", 8)
			continue
		}
		row(pdf, tr, t.Label, t.Value, "", false)
	}
	if v.DiscountNote != "" {
		row(pdf, tr, "Motivo del descuento", v.DiscountNote, "", false)
	}
	if v.Method != "" {
		row(pdf, tr, "Forma de pago", v.Method, "", false)
	}
	if v.OperatorName != "" {
		row(pdf, tr, "Atendió", v.OperatorName, "", false)
	}
	divider(pdf)

	// ── Pie ────────────────────────────────────────────────────────────
	pdf.SetFont("Helvetica", "", 7)
	for _, f := range v.Footer {
		pdf.SetX(left)
		pdf.MultiCell(ticketContent, 3.2, tr(f), "", "C", false)
	}
}

func centered(pdf *gofpdf.Fpdf, tr func(string) string, s string, lineH float64) {
	pdf.SetX(ticketMarginMM)
	pdf.MultiCell(ticketContent, lineH, tr(s), "", "C", false)
}

func divider(pdf *gofpdf.Fpdf) {
	pdf.Ln(1.5)
	y := pdf.GetY()
	pdf.SetDrawColor(120, 120, 120)
	pdf.SetLineWidth(0.2)
	pdf.SetDashPattern([]float64{0.8, 0.8}, 0)
	pdf.Line(ticketMarginMM, y, ticketWidthMM-ticketMarginMM, y)
	pdf.SetDashPattern([]float64{}, 0)
	pdf.Ln(2)
}

// row pinta "etiqueta ......... valor" con la etiqueta en labelStyle y el
// valor en negritas si valueBold. La etiqueta se parte en varias líneas si no
// cabe; un valor muy largo (nombre, plan) baja a su propia línea. Al salir
// deja la fuente en labelStyle con el mismo tamaño.
func row(pdf *gofpdf.Fpdf, tr func(string) string, label, value, labelStyle string, valueBold bool) {
	size, _ := pdf.GetFontSize()    // (pt, unidades) — necesitamos pt
	lineH := size*0.3528*1.25 + 0.4 // pt → mm con interlineado
	valueStyle := labelStyle
	if valueBold {
		valueStyle = "B"
	}
	valueText := tr(value)
	pdf.SetFont("Helvetica", valueStyle, size)
	valueW := pdf.GetStringWidth(valueText) + 1
	pdf.SetFont("Helvetica", labelStyle, size)
	if valueW > ticketContent*0.62 {
		pdf.SetX(ticketMarginMM)
		pdf.CellFormat(ticketContent, lineH, tr(label), "", 1, "L", false, 0, "")
		pdf.SetFont("Helvetica", valueStyle, size)
		pdf.SetX(ticketMarginMM)
		pdf.MultiCell(ticketContent, lineH, valueText, "", "R", false)
		pdf.SetFont("Helvetica", labelStyle, size)
		return
	}
	labelW := ticketContent - valueW
	// SplitLines trabaja por bytes (cp1252 ya traducido); SplitText de
	// gofpdf 1.16 indexa runas y truena con acentos traducidos.
	lines := pdf.SplitLines([]byte(tr(label)), labelW)
	if len(lines) == 0 {
		lines = [][]byte{{}}
	}
	y := pdf.GetY()
	for i, l := range lines {
		pdf.SetXY(ticketMarginMM, y+float64(i)*lineH)
		pdf.CellFormat(labelW, lineH, string(l), "", 0, "L", false, 0, "")
	}
	pdf.SetFont("Helvetica", valueStyle, size)
	pdf.SetXY(ticketMarginMM+labelW, y)
	pdf.CellFormat(valueW, lineH, valueText, "", 0, "R", false, 0, "")
	pdf.SetFont("Helvetica", labelStyle, size)
	pdf.SetXY(ticketMarginMM, y+float64(len(lines))*lineH)
}
