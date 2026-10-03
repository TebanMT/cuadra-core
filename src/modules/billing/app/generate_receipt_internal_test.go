package app

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
)

func TestBuildReceiptLines_CorrectedOvercollectionDoesNotCallPhysicalAmountTotal(t *testing.T) {
	p := &paymentDomain.Payment{
		ID: uuid.New(), GymID: uuid.New(), Folio: "PRD-1", Concept: paymentDomain.ConceptProduct,
		Amount: 40, RecognizedAmount: 4, PaymentMethod: paymentDomain.MethodCash,
		PaymentDate: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
		Breakdown:   []paymentDomain.BreakdownLine{{Label: "Agua", Amount: 4}},
	}
	joined := strings.Join(buildReceiptLines(nil, nil, nil, nil, nil, p, "", ""), "\n")
	for _, want := range []string{
		"Total de venta: $4.00", "Cobrado físicamente: $40.00",
		"Ingreso reconocido en este cobro: $4.00",
		"Diferencia gestionada por corrección/devolución: $36.00",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("receipt missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "Total: $40.00") {
		t.Fatalf("physical overcollection mislabeled as total:\n%s", joined)
	}
}

func TestBuildReceiptLines_ProductDiscountUsesSubtotalOnce(t *testing.T) {
	p := &paymentDomain.Payment{
		ID: uuid.New(), GymID: uuid.New(), Folio: "PRD-2", Concept: paymentDomain.ConceptProduct,
		Amount: 3.60, RecognizedAmount: 3.60, DiscountAmount: 0.40,
		PaymentMethod: paymentDomain.MethodCash,
		PaymentDate:   time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
		Breakdown:     []paymentDomain.BreakdownLine{{Label: "Dulce ×4", Amount: 4}},
	}
	joined := strings.Join(buildReceiptLines(nil, nil, nil, nil, nil, p, "", ""), "\n")
	for _, want := range []string{"Subtotal: $4.00", "Descuento: -$0.40", "Total: $3.60"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("receipt missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "$3.20") {
		t.Fatalf("discount was subtracted twice:\n%s", joined)
	}
}

func TestBuildReceiptLines_FullCreditDoesNotClaimCashWasReceived(t *testing.T) {
	for _, balance := range []float64{40, 25, 0} {
		p := &paymentDomain.Payment{
			Folio: "PRD-3", Concept: paymentDomain.ConceptProduct,
			Amount: 0, RecognizedAmount: 0, BalancePending: balance,
			PaymentMethod: paymentDomain.MethodCash, DiscountAmount: 10,
			PaymentDate: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
			Breakdown:   []paymentDomain.BreakdownLine{{Label: "Agua ×2", Amount: 50}},
		}
		joined := strings.Join(buildReceiptLines(nil, nil, nil, nil, nil, p, "Socio", "SOC-1"), "\n")
		for _, want := range []string{"Comprobante de venta", "Total de venta: $40.00", "Abono inicial: $0.00", "Venta a crédito"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("missing %q: %s", want, joined)
			}
		}
		if strings.Contains(joined, "Método: Efectivo") || strings.Contains(joined, "Comprobante de pago") {
			t.Fatalf("credit receipt claims payment: %s", joined)
		}
	}
}
