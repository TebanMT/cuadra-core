package promotion

import "math"

// CalcInput es el contexto del cobro al que se aplica la promo.
//
//   - Subtotal: monto bruto del cobro antes de descuentos. Incluye plan
//   - enrollment_fee + maintenance_fee (caso membresía) o el total
//     del carrito (caso venta).
//   - EnrollmentFee: monto del enrollment incluído en el subtotal — usado
//     por free_enrollment para descontar exactamente eso.
//   - HasEnrollment: true si el cobro efectivamente está cobrando el
//     enrollment (operator puede haberlo saltado con override). Si es
//     false, free_enrollment es un no-op (discount=0).
type CalcInput struct {
	Subtotal      float64
	EnrollmentFee float64
	HasEnrollment bool
}

// CalcResult es el efecto resuelto de aplicar la promo. Una promo puede
// generar máximo UNO de:
//
//   - Discount: monto en pesos a restar del subtotal (clampeado a [0, subtotal]).
//   - ExtraDays: días a agregar al expiry post-renew.
//   - CompanionCount: cantidad de membresías $0 a regalar a otros socios.
//
// Los 3 campos coexisten en el struct para uniformidad — sólo el que
// corresponde al Kind viene > 0.
type CalcResult struct {
	Discount       float64
	ExtraDays      int
	CompanionCount int
}

// Calculate aplica la promo a un cobro dado. Funcion pura — no consulta
// repos, no efectos. Asume que la promo ya pasó IsCurrentlyValid +
// AppliesToTarget en el caller.
//
// Edge cases:
//   - percent=100 → discount = subtotal (no negativo).
//   - fixed_amount > subtotal → discount = subtotal (clampeado).
//   - free_enrollment sin enrollment efectivo → discount = 0 (no-op).
//   - extra_days con value fraccional → trunca a entero (CHECK >= 1
//     en schema garantiza >= 1).
func Calculate(p *Promotion, in CalcInput) CalcResult {
	if p == nil {
		return CalcResult{}
	}
	switch p.Kind {
	case KindPercent:
		if p.Value == nil {
			return CalcResult{}
		}
		raw := in.Subtotal * (*p.Value) / 100.0
		return CalcResult{Discount: clampDiscount(raw, in.Subtotal)}
	case KindFixedAmount:
		if p.Value == nil {
			return CalcResult{}
		}
		return CalcResult{Discount: clampDiscount(*p.Value, in.Subtotal)}
	case KindFreeEnrollment:
		if !in.HasEnrollment || in.EnrollmentFee <= 0 {
			return CalcResult{}
		}
		return CalcResult{Discount: clampDiscount(in.EnrollmentFee, in.Subtotal)}
	case KindExtraDays:
		if p.Value == nil {
			return CalcResult{}
		}
		days := int(*p.Value)
		if days < 0 {
			days = 0
		}
		return CalcResult{ExtraDays: days}
	case KindCompanionMemberships:
		if p.CompanionCount == nil {
			return CalcResult{}
		}
		return CalcResult{CompanionCount: *p.CompanionCount}
	}
	return CalcResult{}
}

func clampDiscount(d, subtotal float64) float64 {
	if d < 0 {
		return 0
	}
	if d > subtotal {
		return roundCents(subtotal)
	}
	return roundCents(d)
}

func roundCents(v float64) float64 {
	return math.Round(v*100) / 100
}
