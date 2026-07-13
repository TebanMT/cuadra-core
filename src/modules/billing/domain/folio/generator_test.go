package folio

import (
	"testing"

	"github.com/google/uuid"

	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// stubPaymentRepo implementa PaymentRepository devolviendo un maxFolio fijo;
// solo MaxFolioForConcept importa para el generador.
type stubPaymentRepo struct{ maxFolio string }

func (s stubPaymentRepo) MaxFolioForConcept(_ sharedDomain.Transaction, _ uuid.UUID, _ string) (string, error) {
	return s.maxFolio, nil
}
func (stubPaymentRepo) Create(_ sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error) {
	return p, nil
}
func (stubPaymentRepo) Update(_ sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error) {
	return p, nil
}
func (stubPaymentRepo) GetByID(_ sharedDomain.Transaction, _ uuid.UUID) (*paymentDomain.Payment, error) {
	return nil, nil
}
func (stubPaymentRepo) ListByMember(_ sharedDomain.Transaction, _ billingRepo.ListByMemberQuery) ([]*paymentDomain.Payment, int, error) {
	return nil, 0, nil
}
func (stubPaymentRepo) ListByGymBetweenDates(_ sharedDomain.Transaction, _ billingRepo.ListByGymQuery) ([]*paymentDomain.Payment, int, error) {
	return nil, 0, nil
}
func (stubPaymentRepo) HasRefundFor(_ sharedDomain.Transaction, _ uuid.UUID) (bool, error) {
	return false, nil
}
func (stubPaymentRepo) SumPendingByMember(_ sharedDomain.Transaction, _, _ uuid.UUID) (float64, error) {
	return 0, nil
}

func TestGenerator_Next(t *testing.T) {
	gym := uuid.New()
	cases := []struct {
		name string
		max  string
		want string
	}{
		{"sin folios previos → primero", "", "PRD-000001"},
		{"secuencia canónica", "PRD-000041", "PRD-000042"},
		// REGRESIÓN: folio legacy/importado con prefijo distinto al canónico
		// ("PROD-" en vez de "PRD-"). El TrimPrefix(prefix+"-") anterior NO lo
		// quitaba, Sscanf leía 0 → next=1 → generaba "PRD-000001", que ya
		// existía → UNIQUE(gym_id, folio) y la venta fallaba con 500.
		{"prefijo legacy PROD-", "PROD-00005", "PRD-000006"},
		// Folio numérico > 99999 (caso import MEM-%05d): se respeta el valor
		// numérico, no el orden lexicográfico.
		{"numérico sobre 99999", "PRD-100000", "PRD-100001"},
		// Folio legacy sin guion: se toma el folio completo como número.
		{"legacy sin guion", "12345", "PRD-012346"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGenerator(stubPaymentRepo{maxFolio: tc.max})
			got, err := g.Next(nil, gym, paymentDomain.ConceptProduct)
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if got != tc.want {
				t.Errorf("Next(max=%q) = %q, want %q", tc.max, got, tc.want)
			}
		})
	}
}
