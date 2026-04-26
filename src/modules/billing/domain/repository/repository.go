// Package repository declares the persistence contracts for the billing BC.
// Concrete impls live in infraestructure/db/repositories with build tags.
package repository

import (
	"time"

	"github.com/google/uuid"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	saleDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/sale"
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

// SaleRepository — UC-025 (RegisterSale) writes Sale + SaleItems atomically.
// Reads support receipts, refund flows, and reports/cash_close.
type SaleRepository interface {
	Create(tx sharedDomain.Transaction, s *saleDomain.Sale) (*saleDomain.Sale, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*saleDomain.Sale, error)
	GetByPaymentID(tx sharedDomain.Transaction, paymentID uuid.UUID) (*saleDomain.Sale, error)
}

// SaleItemRepository — child rows of a Sale. Carved out so reports can
// run product-level GROUP BYs without loading entire Sales.
type SaleItemRepository interface {
	CreateMany(tx sharedDomain.Transaction, items []*saleDomain.SaleItem) error
	ListBySale(tx sharedDomain.Transaction, saleID uuid.UUID) ([]*saleDomain.SaleItem, error)
}

// CashCloseQuery backs UC-027. Returns the per-method totals + per-concept
// totals + per-operator totals for a single gym/day. The repository turns one
// SQL aggregation into the read model used by the report.
type CashCloseQuery struct {
	GymID uuid.UUID
	Date  time.Time
}

// CashCloseTotals is the read model produced by the repository for UC-027.
// All amounts are signed (refunds are negative). The map keys mirror the
// `payment_method` and `concept` enums.
type CashCloseTotals struct {
	ByMethod    map[string]float64
	ByConcept   map[string]float64
	ByOperator  []OperatorTotal
	GrandTotal  float64
	RefundTotal float64
}

type OperatorTotal struct {
	OperatorID uuid.UUID
	Total      float64
	PaymentsN  int
	SalesN     int
}

// CashCloseReader is the read seam of UC-027. Implementations live alongside
// the SaleRepository (Postgres + SQLite).
type CashCloseReader interface {
	Aggregate(tx sharedDomain.Transaction, q CashCloseQuery) (*CashCloseTotals, error)
}

// CashCloseEventRepository persists the cierre event (UC-027 step 4).
type CashCloseEventRepository interface {
	Create(tx sharedDomain.Transaction, e *cashCloseDomain.CashCloseEvent) (*cashCloseDomain.CashCloseEvent, error)
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]*cashCloseDomain.CashCloseEvent, error)
}
