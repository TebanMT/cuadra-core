package app

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// GenerateReceiptInput backs UC-020 (PDF download). The receipt is rendered
// in-memory; we never store it (DA-20.1).
type GenerateReceiptInput struct {
	GymID     uuid.UUID
	PaymentID uuid.UUID
}

// GenerateReceiptOutput is what the controller streams back. ContentType is
// always "application/pdf"; SuggestedFilename hints to Content-Disposition.
type GenerateReceiptOutput struct {
	PDF               []byte
	ContentType       string
	SuggestedFilename string
}

type GenerateReceipt struct {
	Payments billingRepo.PaymentRepository
	Gyms     gymRepo.GymRepository
	Members  memRepo.MemberRepository
	UoW      sharedDomain.UnitOfWork
}

func NewGenerateReceipt(payments billingRepo.PaymentRepository, gyms gymRepo.GymRepository,
	members memRepo.MemberRepository, uow sharedDomain.UnitOfWork) *GenerateReceipt {
	return &GenerateReceipt{Payments: payments, Gyms: gyms, Members: members, UoW: uow}
}

func (uc *GenerateReceipt) Execute(ctx context.Context, in GenerateReceiptInput) (*GenerateReceiptOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	p, err := uc.Payments.GetByID(tx, in.PaymentID)
	if err != nil {
		return nil, err
	}
	if p.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
	}
	g, err := uc.Gyms.GetByID(tx, in.GymID)
	if err != nil {
		return nil, err
	}
	var memberName, memberFolio string
	if p.MemberID != nil {
		m, err := uc.Members.GetByID(tx, *p.MemberID)
		if err == nil {
			memberName = m.FullName
			memberFolio = m.Folio
		}
	}
	pdf := renderReceipt(g.Name, g.City, g.WhatsApp, g.RFC, g.RazonSocial,
		p, memberName, memberFolio)
	return &GenerateReceiptOutput{
		PDF:               pdf,
		ContentType:       "application/pdf",
		SuggestedFilename: fmt.Sprintf("recibo-%s.pdf", strings.ReplaceAll(p.Folio, "/", "-")),
	}, nil
}

// SendReceiptInput backs the manual UC-020 "Reenviar comprobante" call. El
// envío se delega al seam de notificaciones (EventPublisher →
// BillingEventSubscriber → EnqueueReceipt), el MISMO que dispara el cobro
// automático. Channel/Recipient quedan para overrides futuros (otro número /
// email); hoy el envío default es WhatsApp al socio del pago vía master sender.
type SendReceiptInput struct {
	GymID     uuid.UUID
	PaymentID uuid.UUID
	Channel   string // "whatsapp_member" | "whatsapp_other" | "email_member" | "email_other"
	Recipient string // optional override (phone or email)
}

type SendReceiptOutput struct {
	Status string
	Note   string
}

type SendReceipt struct {
	Payments  billingRepo.PaymentRepository
	Publisher EventPublisher
	UoW       sharedDomain.UnitOfWork
}

func NewSendReceipt(payments billingRepo.PaymentRepository, uow sharedDomain.UnitOfWork) *SendReceipt {
	return &SendReceipt{Payments: payments, Publisher: NoopPublisher{}, UoW: uow}
}

// WithPublisher inyecta el seam de notificaciones (el MISMO
// BillingEventSubscriber que usa el cobro automático para el comprobante).
// Sin él, SendReceipt degrada a NoopPublisher (no-op silencioso) — útil en
// tests que no montan la BC de notificaciones.
func (uc *SendReceipt) WithPublisher(p EventPublisher) *SendReceipt {
	if p != nil {
		uc.Publisher = p
	}
	return uc
}

func (uc *SendReceipt) Execute(ctx context.Context, in SendReceiptInput) (*SendReceiptOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	p, err := uc.Payments.GetByID(tx, in.PaymentID)
	if err != nil {
		return nil, err
	}
	if p.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
	}
	if p.MemberID == nil {
		// Venta anónima sin socio: no hay a quién mandarle el comprobante.
		return &SendReceiptOutput{
			Status: "skipped",
			Note:   "El pago no tiene socio asociado.",
		}, nil
	}
	// Reusa el MISMO seam que el cobro automático (EventPublisher →
	// BillingEventSubscriber → EnqueueReceipt). Idempotente por pago (idemp
	// key receipt:<paymentID>): si el comprobante ya se encoló al cobrar, esto
	// NO duplica; si nunca salió (p.ej. el socio no tenía teléfono al cobrar y
	// se lo agregaron después), lo encola ahora. El dispatcher resuelve el
	// `from` por tier (master sender en Standard). Query() arriba devuelve un
	// tx read-only sin lock (Tx=nil), así que el Command de EnqueueReceipt no choca.
	uc.Publisher.PublishPaymentCompleted(ctx, PaymentCompletedEvent{
		GymID:      p.GymID,
		PaymentID:  in.PaymentID,
		MemberID:   p.MemberID,
		Concept:    p.Concept,
		Amount:     p.Amount,
		Folio:      p.Folio,
		OperatorID: p.OperatorID,
	})
	return &SendReceiptOutput{
		Status: "queued",
		Note:   "Comprobante encolado para envío por WhatsApp.",
	}, nil
}

// ---------------------------------------------------------------------------
// Minimal pure-Go PDF rendering. The output is intentionally simple — single
// page, single font (Helvetica), no images. It satisfies the contract of
// "comprobante on-the-fly" without pulling a third-party PDF dep into go.mod.
// We can swap to gofpdf if the design ever needs richer layout.
// ---------------------------------------------------------------------------

// renderReceipt produces a small valid PDF document with one page of text.
func renderReceipt(gymName, city, whatsapp, rfc, razon *string,
	p *paymentDomain.Payment, memberName, memberFolio string) []byte {
	lines := buildReceiptLines(gymName, city, whatsapp, rfc, razon, p, memberName, memberFolio)
	return buildSimplePDF(lines)
}

func buildReceiptLines(gymName, city, whatsapp, rfc, razon *string,
	p *paymentDomain.Payment, memberName, memberFolio string) []string {
	lines := []string{}
	if gymName != nil {
		lines = append(lines, *gymName)
	} else {
		lines = append(lines, "Gimnasio")
	}
	if city != nil {
		lines = append(lines, *city)
	}
	if whatsapp != nil {
		lines = append(lines, "WhatsApp: "+*whatsapp)
	}
	if rfc != nil {
		lines = append(lines, "RFC: "+*rfc)
	}
	if razon != nil {
		lines = append(lines, *razon)
	}
	lines = append(lines, "")
	lines = append(lines, "Comprobante de pago")
	lines = append(lines, "Folio: "+p.Folio)
	lines = append(lines, "Fecha: "+p.PaymentDate.Format("2006-01-02"))
	if memberName != "" {
		lines = append(lines, "Socio: "+memberName+" ("+memberFolio+")")
	}
	lines = append(lines, "")

	// Sección de conceptos. Si el pago tiene `breakdown` desglosado
	// (UC-018 membership, UC-025 product sale, primer pago) imprimimos
	// línea por línea. Si no (settlements, refunds, pagos pre-migración)
	// caemos al label tradicional de una sola línea con concepto + monto.
	if len(p.Breakdown) > 0 {
		lines = append(lines, "Detalle:")
		for _, l := range p.Breakdown {
			lines = append(lines, fmt.Sprintf("  %-30s $%.2f", truncateLabel(l.Label, 30), l.Amount))
		}
	} else {
		lines = append(lines, "Concepto: "+conceptLabel(p.Concept))
	}

	subtotal := p.Amount + p.DiscountAmount
	if p.DiscountAmount > 0 {
		lines = append(lines, fmt.Sprintf("Subtotal: $%.2f", subtotal))
		lines = append(lines, fmt.Sprintf("Descuento: -$%.2f", p.DiscountAmount))
		if p.DiscountReason != nil {
			lines = append(lines, "Motivo descuento: "+*p.DiscountReason)
		}
	}
	lines = append(lines, fmt.Sprintf("Total: $%.2f", p.Amount))
	if p.BalancePending > 0 {
		lines = append(lines, fmt.Sprintf("Saldo pendiente: $%.2f", p.BalancePending))
	}
	lines = append(lines, "Método: "+methodLabel(p.PaymentMethod))

	lines = append(lines, "")
	lines = append(lines, "Este NO es un comprobante fiscal.")
	lines = append(lines, "Para factura, comuníquese con el gimnasio.")
	return lines
}

// truncateLabel mantiene el ancho fijo del PDF razonable (Helvetica 12pt
// no es monoespacio pero para texto breve queda alineado a ojo). Si una
// etiqueta excede `max`, recortamos con elipsis.
func truncateLabel(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func conceptLabel(c string) string {
	switch c {
	case paymentDomain.ConceptMembership:
		return "Mensualidad / membresía"
	case paymentDomain.ConceptProduct:
		return "Venta de producto"
	case paymentDomain.ConceptBalanceSettlement:
		return "Liquidación de saldo"
	case paymentDomain.ConceptRefund:
		return "Cancelación / devolución"
	}
	return c
}

func methodLabel(m string) string {
	switch m {
	case paymentDomain.MethodCash:
		return "Efectivo"
	case paymentDomain.MethodTransfer:
		return "Transferencia"
	case paymentDomain.MethodCard:
		return "Tarjeta"
	}
	return m
}

// buildSimplePDF emits a valid PDF 1.4 document with a single page and one
// text stream. We render at 12pt Helvetica with 16pt leading, top-left
// anchored at (50, 760) on Letter (612 x 792 points).
//
// The PDF spec is forgiving enough that this minimal structure renders in any
// reader. We escape backslash, parens, and non-ASCII chars (the latter as
// a fallback to "?" to keep WinAnsi-safe; for receipts in es-MX with diacritics
// we transliterate the most common accented chars).
func buildSimplePDF(lines []string) []byte {
	var stream bytes.Buffer
	stream.WriteString("BT\n")
	stream.WriteString("/F1 12 Tf\n")
	stream.WriteString("50 760 Td\n")
	for i, line := range lines {
		safe := pdfSafeText(line)
		if i > 0 {
			stream.WriteString("0 -16 Td\n")
		}
		stream.WriteString("(")
		stream.WriteString(safe)
		stream.WriteString(") Tj\n")
	}
	stream.WriteString("ET\n")
	streamBytes := stream.Bytes()

	var doc bytes.Buffer
	doc.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")

	offsets := []int{}
	add := func(s string) {
		offsets = append(offsets, doc.Len())
		doc.WriteString(s)
	}
	add("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	add("2 0 obj\n<< /Type /Pages /Count 1 /Kids [3 0 R] >>\nendobj\n")
	add(fmt.Sprintf("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>\nendobj\n"))
	add(fmt.Sprintf("4 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(streamBytes), streamBytes))
	add("5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>\nendobj\n")

	xrefStart := doc.Len()
	doc.WriteString(fmt.Sprintf("xref\n0 %d\n", len(offsets)+1))
	doc.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		doc.WriteString(fmt.Sprintf("%010d 00000 n \n", off))
	}
	doc.WriteString(fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefStart))
	return doc.Bytes()
}

// pdfSafeText escapes ()\\ and substitutes a small set of es-MX accented
// codepoints for their WinAnsi equivalents (so "é", "ñ" survive). Everything
// else outside ASCII is replaced with '?'.
func pdfSafeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '(':
			b.WriteString(`\(`)
		case ')':
			b.WriteString(`\)`)
		default:
			if r < 0x80 {
				b.WriteRune(r)
				continue
			}
			if w, ok := winAnsiSubst[r]; ok {
				b.WriteByte(w)
				continue
			}
			b.WriteByte('?')
		}
	}
	return b.String()
}

// winAnsiSubst maps the most common es-MX glyphs to their WinAnsi byte values.
var winAnsiSubst = map[rune]byte{
	'á': 0xE1, 'é': 0xE9, 'í': 0xED, 'ó': 0xF3, 'ú': 0xFA,
	'Á': 0xC1, 'É': 0xC9, 'Í': 0xCD, 'Ó': 0xD3, 'Ú': 0xDA,
	'ñ': 0xF1, 'Ñ': 0xD1,
	'ü': 0xFC, 'Ü': 0xDC,
	'¡': 0xA1, '¿': 0xBF,
	'°': 0xB0,
	// fallback used by string substitutions
}
