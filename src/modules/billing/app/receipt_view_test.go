package app

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
)

func strp(s string) *string { return &s }

func membershipPayment(lines ...paymentDomain.BreakdownLine) *paymentDomain.Payment {
	return &paymentDomain.Payment{
		ID: uuid.New(), GymID: uuid.New(), Folio: "PAG-0042",
		Concept: paymentDomain.ConceptMembership, Amount: 650, RecognizedAmount: 650,
		PaymentMethod: paymentDomain.MethodCash,
		PaymentDate:   time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
		// 00:42 UTC del 28 = 18:42 del 27 en CDMX.
		CreatedAt: time.Date(2026, 11, 28, 0, 42, 0, 0, time.UTC),
		Breakdown: lines,
	}
}

func testGym() *gymDomain.Gym {
	return &gymDomain.Gym{
		ID: uuid.New(), Name: strp("Gym Centro"), City: strp("Puebla"), CodigoPostal: strp("72000"),
		RFC: strp("GCE010101AAA"), Timezone: "America/Mexico_City",
	}
}

func TestReceiptView_EnrollmentWithVigenciaOperatorAndLocalTime(t *testing.T) {
	n := 1234
	expiry := time.Date(2026, 12, 27, 0, 0, 0, 0, time.UTC)
	p := membershipPayment(
		paymentDomain.BreakdownLine{Label: "Mensual", Amount: 450},
		paymentDomain.BreakdownLine{Label: "Inscripción", Amount: 200},
	)
	v := buildReceiptView(receiptInputs{
		gym: testGym(), payment: p,
		member:       &memberDomain.Member{FullName: "Ana López", MemberNumber: &n, Folio: "SOC-7"},
		membership:   &membershipDomain.Membership{TypeNameSnapshot: "Mensual", StartDate: p.PaymentDate, ExpiryDate: &expiry},
		operatorName: "Luis Recepción",
	})
	if v.Kind != ReceiptKindEnrollment || v.Title != "Comprobante de inscripción" {
		t.Fatalf("kind/title = %s / %s", v.Kind, v.Title)
	}
	if v.Date != "27 nov 2026" || v.Time != "18:42" || v.RegisteredAt != "" {
		t.Fatalf("date/time = %q %q %q", v.Date, v.Time, v.RegisteredAt)
	}
	if v.Membership == nil || v.Membership.StartDate != "27 nov 2026" || v.Membership.ExpiryDate != "27 dic 2026" {
		t.Fatalf("membership = %+v", v.Membership)
	}
	if v.Member == nil || v.Member.MemberNumber != "1234" || v.OperatorName != "Luis Recepción" {
		t.Fatalf("member/operator = %+v %q", v.Member, v.OperatorName)
	}
	if v.Gym.Address != "Puebla, C.P. 72000" {
		t.Fatalf("address = %q", v.Gym.Address)
	}
	last := v.Totals[len(v.Totals)-1]
	if last.Label != "Total" || last.Value != "$650.00" || !last.Emphasis {
		t.Fatalf("total = %+v", last)
	}
	if !strings.Contains(strings.Join(v.Footer, " "), "Conserva este comprobante") {
		t.Fatalf("footer = %v", v.Footer)
	}
}

func TestReceiptView_RenewalVsFirstMembership(t *testing.T) {
	p := membershipPayment(paymentDomain.BreakdownLine{Label: "Mensual", Amount: 650})
	if v := buildReceiptView(receiptInputs{gym: testGym(), payment: p}); v.Kind != ReceiptKindRenewal {
		t.Fatalf("sin inscripción y con historial = renovación, got %s", v.Kind)
	}
	// Gym que no cobra inscripción: la primera membresía sigue siendo
	// inscripción.
	if v := buildReceiptView(receiptInputs{gym: testGym(), payment: p, firstMembership: true}); v.Kind != ReceiptKindEnrollment {
		t.Fatalf("primera membresía = inscripción, got %s", v.Kind)
	}
	plan := membershipPayment(paymentDomain.BreakdownLine{Label: "Cambio de plan — Anual", Amount: 650})
	if v := buildReceiptView(receiptInputs{gym: testGym(), payment: plan, firstMembership: true}); v.Kind != ReceiptKindPlanChange {
		t.Fatalf("cambio de plan, got %s", v.Kind)
	}
}

func TestReceiptView_BackdatedPaymentShowsRegistrationMoment(t *testing.T) {
	p := membershipPayment(paymentDomain.BreakdownLine{Label: "Mensual", Amount: 650})
	p.PaymentDate = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	v := buildReceiptView(receiptInputs{gym: testGym(), payment: p})
	if v.Time != "" || v.RegisteredAt != "27 nov 2026, 18:42" {
		t.Fatalf("time=%q registered=%q", v.Time, v.RegisteredAt)
	}
}

func TestReceiptView_PartialPaymentAndRefundLabels(t *testing.T) {
	p := membershipPayment(paymentDomain.BreakdownLine{Label: "Mensual", Amount: 650})
	p.Amount, p.BalancePending = 400, 250
	v := buildReceiptView(receiptInputs{gym: testGym(), payment: p})
	joined := strings.Join(receiptTextLines(v), "\n")
	for _, want := range []string{"Total pagado: $400.00", "Saldo pendiente: $250.00"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q:\n%s", want, joined)
		}
	}
	r := &paymentDomain.Payment{Folio: "DEV-1", Concept: paymentDomain.ConceptRefund, Amount: -1450.5,
		PaymentMethod: paymentDomain.MethodCash, PaymentDate: p.PaymentDate}
	rv := buildReceiptView(receiptInputs{payment: r})
	if rv.Title != "Comprobante de devolución" || rv.Totals[0].Value != "$1,450.50" {
		t.Fatalf("refund = %s %+v", rv.Title, rv.Totals)
	}
}

func TestFormatMoney(t *testing.T) {
	for in, want := range map[float64]string{0: "$0.00", 12.5: "$12.50", 1234567.891: "$1,234,567.89", -40: "-$40.00", 999.999: "$1,000.00"} {
		if got := formatMoney(in); got != want {
			t.Errorf("formatMoney(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderReceiptPDF_TicketWidthWithLogo(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for x := 0; x < 40; x++ {
		img.Set(x, 10, color.Black)
	}
	var logo bytes.Buffer
	if err := png.Encode(&logo, img); err != nil {
		t.Fatal(err)
	}
	n := 1234
	expiry := time.Date(2026, 12, 27, 0, 0, 0, 0, time.UTC)
	p := membershipPayment(
		paymentDomain.BreakdownLine{Label: "Mensualidad con un nombre de plan muy, muy largo para partir", Amount: 450},
		paymentDomain.BreakdownLine{Label: "Inscripción", Amount: 200},
	)
	v := buildReceiptView(receiptInputs{
		gym: testGym(), payment: p, hasLogo: true, operatorName: "Luis",
		member:     &memberDomain.Member{FullName: "María José Hernández de la Peña Sánchez", MemberNumber: &n},
		membership: &membershipDomain.Membership{TypeNameSnapshot: "Mensual", StartDate: p.PaymentDate, ExpiryDate: &expiry},
	})
	for _, withLogo := range [][]byte{logo.Bytes(), nil, []byte("no es png")} {
		out, err := renderReceiptPDF(v, withLogo)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if !bytes.HasPrefix(out, []byte("%PDF-")) {
			t.Fatal("no es un PDF")
		}
		// 80 mm = 226.77 pt de ancho.
		if !bytes.Contains(out, []byte("/MediaBox [0 0 226.77")) {
			t.Fatalf("el PDF debería tener ancho de ticket de 80 mm")
		}
	}
}
