// Package repository declares the persistence contracts for the billing BC.
// Concrete impls live in infraestructure/db/repositories with build tags.
package repository

import (
	"time"

	"github.com/google/uuid"

	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// PaymentRepository is the persistence contract for `payments`. Append-only:
// `Update` is intentionally restricted to mutate only `notes` and
// `balance_pending` (plus the bookkeeping `version` / `updated_at`). Refunds
// and settlements are NEW rows; never UPDATE the historical row's amount.
type PaymentRepository interface {
	Create(tx sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error)
	Update(tx sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*paymentDomain.Payment, error)
	ListByMember(tx sharedDomain.Transaction, q ListByMemberQuery) ([]*paymentDomain.Payment, int, error)
	ListByGymBetweenDates(tx sharedDomain.Transaction, q ListByGymQuery) ([]*paymentDomain.Payment, int, error)
	// HasRefundFor reports whether a UC-022 refund row already references the
	// given parent payment. Used to enforce DA-22.1 single-refund.
	HasRefundFor(tx sharedDomain.Transaction, parentPaymentID uuid.UUID) (bool, error)
	// MaxFolioForConcept returns the most-recent folio (by lexicographic max)
	// for a given (gym, concept) — used by FolioGenerator. Empty string when
	// none exists. Implementations MUST take a row lock so concurrent
	// transactions serialise (Postgres: FOR UPDATE).
	MaxFolioForConcept(tx sharedDomain.Transaction, gymID uuid.UUID, concept string) (string, error)
}

// ListByMemberQuery backs UC-021. ConceptFilter is one of "" (all),
// "membership", "product", "balance_settlement", "refund", "other".
type ListByMemberQuery struct {
	GymID         uuid.UUID
	MemberID      uuid.UUID
	ConceptFilter string
	From          *time.Time
	To            *time.Time
	Page          int
	PageSize      int
}

// ListByGymQuery is a generic gym-scoped listing used by reports and the
// future cash-close flow.
type ListByGymQuery struct {
	GymID         uuid.UUID
	From          time.Time
	To            time.Time
	ConceptFilter string
	Page          int
	PageSize      int
}
