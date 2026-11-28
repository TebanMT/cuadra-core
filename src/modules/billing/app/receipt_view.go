package app

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// Receipt kinds — qué tipo de comprobante es. El de inscripción/renovación
// es el que más reclamos resuelve ("¿cuándo me vence?"), por eso la vigencia
// va destacada.
const (
	ReceiptKindEnrollment = "membership_enrollment"
	ReceiptKindRenewal    = "membership_renewal"
	ReceiptKindPlanChange = "plan_change"
	ReceiptKindProduct    = "product"
	ReceiptKindCreditSale = "credit_sale"
	ReceiptKindSettlement = "balance_settlement"
	ReceiptKindRefund     = "refund"
	ReceiptKindOther      = "other"
)

// ReceiptRow es una fila etiqueta/valor ya formateada.
type ReceiptRow struct {
	Label    string `json:"label"`
	Value    string `json:"value"`
	Emphasis bool   `json:"emphasis,omitempty"`
}

type ReceiptGymInfo struct {
	Name        string `json:"name"`
	RazonSocial string `json:"razon_social,omitempty"`
	RFC         string `json:"rfc,omitempty"`
	Address     string `json:"address,omitempty"`
	WhatsApp    string `json:"whatsapp,omitempty"`
	HasLogo     bool   `json:"has_logo"`
}

type ReceiptMemberInfo struct {
	FullName     string `json:"full_name"`
	MemberNumber string `json:"member_number,omitempty"`
	Folio        string `json:"folio,omitempty"`
}

type ReceiptMembershipInfo struct {
	PlanName   string `json:"plan_name"`
	StartDate  string `json:"start_date"`
	ExpiryDate string `json:"expiry_date,omitempty"`
}

// ReceiptView es la ÚNICA fuente del contenido del comprobante: el ticket
// impreso del desktop, el del dashboard y el PDF que viaja por WhatsApp se
// arman de aquí, así que siempre dicen lo mismo. Todo viene formateado en
// español y en la zona horaria del gym; los clientes sólo maquetan.
type ReceiptView struct {
	PaymentID    uuid.UUID              `json:"payment_id"`
	Kind         string                 `json:"kind"`
	Title        string                 `json:"title"`
	Gym          ReceiptGymInfo         `json:"gym"`
	Folio        string                 `json:"folio"`
	Date         string                 `json:"date"`
	Time         string                 `json:"time,omitempty"`
	RegisteredAt string                 `json:"registered_at,omitempty"`
	Member       *ReceiptMemberInfo     `json:"member,omitempty"`
	Membership   *ReceiptMembershipInfo `json:"membership,omitempty"`
	Lines        []ReceiptRow           `json:"lines"`
	Totals       []ReceiptRow           `json:"totals"`
	DiscountNote string                 `json:"discount_note,omitempty"`
	Method       string                 `json:"method,omitempty"`
	OperatorName string                 `json:"operator_name,omitempty"`
	Footer       []string               `json:"footer"`
}

// receiptInputs agrupa lo que el builder necesita; todo opcional salvo el
// pago, para que un dato faltante (socio borrado, operador sin sincronizar)
// degrade a "sin esa línea" en vez de tumbar el comprobante.
type receiptInputs struct {
	gym             *gymDomain.Gym
	payment         *paymentDomain.Payment
	member          *memberDomain.Member
	membership      *membershipDomain.Membership
	firstMembership bool
	operatorName    string
	hasLogo         bool
}

func buildReceiptView(in receiptInputs) ReceiptView {
	p := in.payment
	v := ReceiptView{
		PaymentID:    p.ID,
		Folio:        p.Folio,
		Lines:        []ReceiptRow{},
		Totals:       []ReceiptRow{},
		OperatorName: strings.TrimSpace(in.operatorName),
	}

	tzName := ""
	if in.gym != nil {
		tzName = in.gym.Timezone
		v.Gym = ReceiptGymInfo{
			Name:        derefTrim(in.gym.Name),
			RazonSocial: derefTrim(in.gym.RazonSocial),
			RFC:         derefTrim(in.gym.RFC),
			WhatsApp:    derefTrim(in.gym.WhatsApp),
			HasLogo:     in.hasLogo,
		}
		addr := derefTrim(in.gym.City)
		if cp := derefTrim(in.gym.CodigoPostal); cp != "" {
			if addr != "" {
				addr += ", "
			}
			addr += "C.P. " + cp
		}
		v.Gym.Address = addr
	}
	if v.Gym.Name == "" {
		v.Gym.Name = "Gimnasio"
	}

	// Fecha de negocio (date-only, día local del gym) + hora local de
	// registro. Si el cobro se registró otro día (fecha retroactiva), lo
	// decimos explícito en lugar de pegar una hora que no corresponde.
	v.Date = formatReceiptDate(p.PaymentDate)
	if !p.CreatedAt.IsZero() {
		local := p.CreatedAt.In(tz.LocationOrUTC(tzName))
		if sameCalendarDay(local, p.PaymentDate) {
			v.Time = local.Format("15:04")
		} else {
			v.RegisteredAt = formatReceiptDate(time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)) + ", " + local.Format("15:04")
		}
	}

	if in.member != nil {
		mi := ReceiptMemberInfo{FullName: strings.TrimSpace(in.member.FullName), Folio: in.member.Folio}
		if in.member.MemberNumber != nil {
			mi.MemberNumber = fmt.Sprintf("%d", *in.member.MemberNumber)
		}
		v.Member = &mi
	}
	if in.membership != nil {
		ms := ReceiptMembershipInfo{
			PlanName:  strings.TrimSpace(in.membership.TypeNameSnapshot),
			StartDate: formatReceiptDate(in.membership.StartDate),
		}
		if in.membership.ExpiryDate != nil {
			ms.ExpiryDate = formatReceiptDate(*in.membership.ExpiryDate)
		}
		v.Membership = &ms
	}

	creditSale := p.Concept == paymentDomain.ConceptProduct && p.Amount == 0
	v.Kind, v.Title = receiptKindTitle(p, creditSale, in.firstMembership)

	for _, l := range p.Breakdown {
		v.Lines = append(v.Lines, ReceiptRow{Label: l.Label, Value: formatMoney(l.Amount)})
	}
	if len(v.Lines) == 0 {
		label := conceptLabel(p.Concept)
		if v.Membership != nil && v.Membership.PlanName != "" && p.Concept == paymentDomain.ConceptMembership {
			label = v.Membership.PlanName
		}
		v.Lines = append(v.Lines, ReceiptRow{Label: label, Value: formatMoney(p.Amount)})
	}

	v.Totals = receiptTotals(p, creditSale)
	if p.DiscountAmount > 0 && p.DiscountReason != nil && strings.TrimSpace(*p.DiscountReason) != "" {
		v.DiscountNote = strings.TrimSpace(*p.DiscountReason)
	}
	if creditSale {
		v.Method = "Venta a crédito"
	} else {
		v.Method = methodLabel(p.PaymentMethod)
	}

	v.Footer = []string{
		"Este no es un comprobante fiscal.",
		"Para factura, comunícate con el gimnasio.",
	}
	if v.Kind == ReceiptKindEnrollment || v.Kind == ReceiptKindRenewal || v.Kind == ReceiptKindPlanChange {
		v.Footer = append([]string{"Conserva este comprobante como respaldo de tu pago y tu vigencia."}, v.Footer...)
	}
	return v
}

func receiptKindTitle(p *paymentDomain.Payment, creditSale, firstMembership bool) (string, string) {
	switch p.Concept {
	case paymentDomain.ConceptMembership:
		for _, l := range p.Breakdown {
			if strings.HasPrefix(l.Label, "Cambio de plan") {
				return ReceiptKindPlanChange, "Comprobante de cambio de plan"
			}
		}
		for _, l := range p.Breakdown {
			if strings.EqualFold(strings.TrimSpace(l.Label), "Inscripción") {
				return ReceiptKindEnrollment, "Comprobante de inscripción"
			}
		}
		if firstMembership {
			return ReceiptKindEnrollment, "Comprobante de inscripción"
		}
		return ReceiptKindRenewal, "Comprobante de renovación"
	case paymentDomain.ConceptProduct:
		if creditSale {
			return ReceiptKindCreditSale, "Comprobante de venta"
		}
		return ReceiptKindProduct, "Comprobante de pago"
	case paymentDomain.ConceptBalanceSettlement:
		return ReceiptKindSettlement, "Comprobante de abono"
	case paymentDomain.ConceptRefund:
		return ReceiptKindRefund, "Comprobante de devolución"
	}
	return ReceiptKindOther, "Comprobante de pago"
}

// receiptTotals conserva la semántica de dinero que ya cubren las pruebas
// del comprobante: ventas a crédito, cobros corregidos (físico vs ingreso
// reconocido) y descuentos aplicados una sola vez.
func receiptTotals(p *paymentDomain.Payment, creditSale bool) []ReceiptRow {
	rows := []ReceiptRow{}
	var breakdownTotal float64
	for _, line := range p.Breakdown {
		breakdownTotal += line.Amount
	}
	subtotal := p.Amount + p.BalancePending + p.DiscountAmount
	if breakdownTotal > 0 {
		subtotal = breakdownTotal
	}
	correctedSaleTotal := breakdownTotal - p.DiscountAmount
	if p.DiscountAmount > 0 {
		rows = append(rows,
			ReceiptRow{Label: "Subtotal", Value: formatMoney(subtotal)},
			ReceiptRow{Label: "Descuento", Value: formatMoney(-p.DiscountAmount)},
		)
	}
	switch {
	case creditSale:
		rows = append(rows,
			ReceiptRow{Label: "Total de venta", Value: formatMoney(subtotal - p.DiscountAmount), Emphasis: true},
			ReceiptRow{Label: "Abono inicial", Value: formatMoney(0)},
		)
	case p.Concept == paymentDomain.ConceptProduct && len(p.Breakdown) > 0 &&
		(!sameMoney(correctedSaleTotal, p.Amount) || !sameMoney(p.RecognizedAmount, p.Amount)):
		rows = append(rows,
			ReceiptRow{Label: "Total de venta", Value: formatMoney(correctedSaleTotal), Emphasis: true},
			ReceiptRow{Label: "Cobrado físicamente", Value: formatMoney(p.Amount)},
			ReceiptRow{Label: "Ingreso reconocido en este cobro", Value: formatMoney(p.RecognizedAmount)},
		)
		if difference := p.Amount - p.RecognizedAmount; difference > 0.004 {
			rows = append(rows, ReceiptRow{Label: "Diferencia gestionada por corrección/devolución", Value: formatMoney(difference)})
		}
	case p.Concept == paymentDomain.ConceptRefund:
		rows = append(rows, ReceiptRow{Label: "Total devuelto", Value: formatMoney(math.Abs(p.Amount)), Emphasis: true})
	case p.BalancePending > 0:
		rows = append(rows, ReceiptRow{Label: "Total pagado", Value: formatMoney(p.Amount), Emphasis: true})
	default:
		rows = append(rows, ReceiptRow{Label: "Total", Value: formatMoney(p.Amount), Emphasis: true})
	}
	if p.BalancePending > 0 {
		rows = append(rows, ReceiptRow{Label: "Saldo pendiente", Value: formatMoney(p.BalancePending)})
	}
	return rows
}

// formatMoney → "$1,234.50" / "-$40.00" (es-MX: coma de miles, punto decimal).
func formatMoney(v float64) string {
	c := int64(math.Round(v * 100))
	sign := ""
	if c < 0 {
		sign = "-"
		c = -c
	}
	whole, frac := c/100, c%100
	s := fmt.Sprintf("%d", whole)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return fmt.Sprintf("%s$%s.%02d", sign, b.String(), frac)
}

var receiptMonths = [...]string{"", "ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sep", "oct", "nov", "dic"}

// formatReceiptDate formatea una fecha de negocio (date-only anclada a
// medianoche UTC del día local del gym) → "27 nov 2026".
func formatReceiptDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.UTC()
	return fmt.Sprintf("%d %s %d", t.Day(), receiptMonths[t.Month()], t.Year())
}

func sameCalendarDay(local, businessDate time.Time) bool {
	b := businessDate.UTC()
	return local.Year() == b.Year() && local.Month() == b.Month() && local.Day() == b.Day()
}

func derefTrim(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// receiptTextLines aplana la vista a texto plano (log, pruebas y el
// fallback de email).
func receiptTextLines(v ReceiptView) []string {
	lines := []string{v.Gym.Name}
	for _, s := range []string{v.Gym.RazonSocial, v.Gym.Address} {
		if s != "" {
			lines = append(lines, s)
		}
	}
	if v.Gym.RFC != "" {
		lines = append(lines, "RFC: "+v.Gym.RFC)
	}
	if v.Gym.WhatsApp != "" {
		lines = append(lines, "WhatsApp: "+v.Gym.WhatsApp)
	}
	lines = append(lines, "", v.Title, "Folio: "+v.Folio)
	date := "Fecha: " + v.Date
	if v.Time != "" {
		date += " " + v.Time
	}
	lines = append(lines, date)
	if v.RegisteredAt != "" {
		lines = append(lines, "Registrado: "+v.RegisteredAt)
	}
	if v.Member != nil {
		s := "Socio: " + v.Member.FullName
		if v.Member.MemberNumber != "" {
			s += " (No. " + v.Member.MemberNumber + ")"
		} else if v.Member.Folio != "" {
			s += " (" + v.Member.Folio + ")"
		}
		lines = append(lines, s)
	}
	if v.Membership != nil {
		lines = append(lines, "Plan: "+v.Membership.PlanName, "Inicio: "+v.Membership.StartDate)
		if v.Membership.ExpiryDate != "" {
			lines = append(lines, "Vence: "+v.Membership.ExpiryDate)
		}
	}
	lines = append(lines, "", "Detalle:")
	for _, r := range v.Lines {
		lines = append(lines, "  "+r.Label+" "+r.Value)
	}
	for _, r := range v.Totals {
		lines = append(lines, r.Label+": "+r.Value)
	}
	if v.DiscountNote != "" {
		lines = append(lines, "Motivo descuento: "+v.DiscountNote)
	}
	if v.Method == "Venta a crédito" {
		lines = append(lines, v.Method)
	} else if v.Method != "" {
		lines = append(lines, "Método: "+v.Method)
	}
	if v.OperatorName != "" {
		lines = append(lines, "Atendió: "+v.OperatorName)
	}
	lines = append(lines, "")
	lines = append(lines, v.Footer...)
	return lines
}
