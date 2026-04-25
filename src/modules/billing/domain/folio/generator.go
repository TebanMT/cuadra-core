// Package folio holds the FolioGenerator domain service. Folios are
// gym-scoped + concept-scoped sequences printed on receipts. Format:
//
//	concept=membership          -> MEM-NNNNNN
//	concept=product             -> PRD-NNNNNN
//	concept=balance_settlement  -> BAL-NNNNNN
//	concept=refund              -> RFD-NNNNNN
//	concept=other               -> OTH-NNNNNN
//
// Concurrency is the repository's job (FOR UPDATE on Postgres, the
// single-writer SQLite tx serializes naturally).
package folio

import (
	"fmt"
	"strings"

	"github.com/google/uuid"

	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Generator is the domain service. It depends only on the PaymentRepository
// (specifically MaxFolioForConcept) so we don't introduce a parallel
// gym_folios table for MVP.
type Generator struct {
	Payments billingRepo.PaymentRepository
}

func NewGenerator(payments billingRepo.PaymentRepository) *Generator {
	return &Generator{Payments: payments}
}

// Next returns the next folio for `gymID` + `concept`, formatted as
// PREFIX-NNNNNN. It MUST be called inside a Command transaction so the
// underlying lock holds until the matching INSERT lands.
func (g *Generator) Next(tx sharedDomain.Transaction, gymID uuid.UUID, concept string) (string, error) {
	prefix, err := PrefixForConcept(concept)
	if err != nil {
		return "", err
	}
	maxFolio, err := g.Payments.MaxFolioForConcept(tx, gymID, concept)
	if err != nil {
		return "", err
	}
	next := 1
	if maxFolio != "" {
		s := strings.TrimPrefix(maxFolio, prefix+"-")
		var n int
		_, _ = fmt.Sscanf(s, "%d", &n)
		next = n + 1
	}
	return fmt.Sprintf("%s-%06d", prefix, next), nil
}

// PrefixForConcept maps a concept string to its receipt-folio prefix.
func PrefixForConcept(concept string) (string, error) {
	switch concept {
	case paymentDomain.ConceptMembership:
		return "MEM", nil
	case paymentDomain.ConceptProduct:
		return "PRD", nil
	case paymentDomain.ConceptBalanceSettlement:
		return "BAL", nil
	case paymentDomain.ConceptRefund:
		return "RFD", nil
	case paymentDomain.ConceptOther:
		return "OTH", nil
	}
	return "", sharedDomain.NewBusinessError(errInvalidConcept, "")
}

var errInvalidConcept = fmtErr("concepto inválido para folio")

type sentinel string

func (s sentinel) Error() string { return string(s) }
func fmtErr(msg string) error    { return sentinel(msg) }
