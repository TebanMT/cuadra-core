// Package payment holds the Payment aggregate. Append-only by convention:
// the only mutable fields after creation are `notes` and `balance_pending`
// (the latter is decremented by UC-019 settlements). Refunds and settlements
// are NEW rows that point at the parent via parent_payment_id (see ADR-002
// §3.9 and UC-018..UC-022 for the full model).
package payment

import (
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
)

// PaymentMethod values mirror chk_payments_method.
const (
	MethodCash     = "cash"
	MethodTransfer = "transfer"
	MethodCard     = "card"
)

// Concept values mirror chk_payments_concept.
const (
	ConceptMembership        = "membership"
	ConceptProduct           = "product"
	ConceptBalanceSettlement = "balance_settlement"
	ConceptRefund            = "refund"
	ConceptOther             = "other"
)

// BreakdownLine is one component of a Payment's subtotal. UC-018 stores
// (plan, enrollment_fee, maintenance_fee) on a membership renewal; UC-025
// stores (product_name × qty) per item. The receipt renderer prints these
// lines verbatim under the total.
//
// Labels are caller-formatted (e.g. "Mensualidad", "Inscripción",
// "Proteína 1kg ×2") so the domain doesn't need a translation table.
type BreakdownLine struct {
	Label  string  `json:"label"`
	Amount float64 `json:"amount"`
}

// Payment is the central aggregate of the billing BC. Money is kept as
// float64 here for ergonomics; the SQLite mapper converts to cents at the
// edge. Negative `amount` is reserved for refunds (DA-22.1).
type Payment struct {
	ID              uuid.UUID
	GymID           uuid.UUID
	Version         int
	Folio           string
	MemberID        *uuid.UUID
	Amount          float64
	PaymentMethod   string
	Concept         string
	ParentPaymentID *uuid.UUID
	DiscountAmount  float64
	DiscountReason  *string
	BalancePending  float64
	PaymentDate     time.Time
	Notes           *string
	// Breakdown desglosa el subtotal en líneas individuales. Vacío =
	// pago "atómico" (settlements, refunds, casos donde la línea única
	// es suficiente); el renderizador del PDF cae al label tradicional.
	Breakdown  []BreakdownLine
	OperatorID uuid.UUID
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeletedAt  *time.Time
}

// SetBreakdown attaches the per-concept lines. Returns the payment for
// chaining at construction time; callers (UC-018, UC-025, CreateMember
// first-pay) populate this after NewMembershipPayment / NewProductSalePayment.
func (p *Payment) SetBreakdown(lines []BreakdownLine) *Payment {
	p.Breakdown = lines
	return p
}

// NewMembershipPayment builds a new Payment row for UC-018. Caller passes the
// fully-resolved subtotal, discount, balance, etc. — invariants are checked
// here rather than re-derived. `discount` may be 0; `discountReason` is
// required when discount > 0 (DA-18.2).
func NewMembershipPayment(
	id, gymID, operatorID uuid.UUID,
	memberID uuid.UUID,
	folio string,
	subtotal, discount, paid, balancePending float64,
	method string,
	paymentDate, now time.Time,
	notes *string,
	discountReason *string,
) (*Payment, error) {
	if !validMethod(method) {
		if method == "" {
			return nil, billingErrors.ErrPaymentMethodMissing
		}
		return nil, billingErrors.ErrPaymentMethodInvalid
	}
	if subtotal <= 0 {
		return nil, billingErrors.ErrAmountInvalid
	}
	if discount < 0 || discount >= subtotal {
		return nil, billingErrors.ErrDiscountTooLarge
	}
	if discount > 0 {
		if discountReason == nil || strings.TrimSpace(*discountReason) == "" {
			return nil, billingErrors.ErrDiscountReasonRequired
		}
		clean := strings.TrimSpace(*discountReason)
		discountReason = &clean
	} else {
		discountReason = nil
	}
	total := subtotal - discount
	if paid <= 0 || paid > total {
		return nil, billingErrors.ErrPartialAmountInvalid
	}
	if balancePending < 0 || roundCents(paid+balancePending) != roundCents(total) {
		return nil, billingErrors.ErrPartialAmountInvalid
	}
	if err := validateNotes(notes); err != nil {
		return nil, err
	}
	if paymentDate.IsZero() {
		return nil, billingErrors.ErrPaymentDateInvalid
	}
	mID := memberID
	return &Payment{
		ID:             id,
		GymID:          gymID,
		Version:        1,
		Folio:          folio,
		MemberID:       &mID,
		Amount:         roundCents(paid),
		PaymentMethod:  method,
		Concept:        ConceptMembership,
		DiscountAmount: roundCents(discount),
		DiscountReason: discountReason,
		BalancePending: roundCents(balancePending),
		PaymentDate:    truncateDate(paymentDate),
		Notes:          notes,
		OperatorID:     operatorID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// NewBalanceSettlementPayment builds a UC-019 settlement Payment. `amount` is
// what the operator just collected — it must be > 0 and ≤ remaining balance.
// The caller validates against the parent and decrements parent.BalancePending
// before persisting both rows.
func NewBalanceSettlementPayment(
	id, gymID, operatorID uuid.UUID,
	parent *Payment,
	folio string,
	amount float64,
	method string,
	paymentDate, now time.Time,
	notes *string,
) (*Payment, error) {
	if !validMethod(method) {
		if method == "" {
			return nil, billingErrors.ErrPaymentMethodMissing
		}
		return nil, billingErrors.ErrPaymentMethodInvalid
	}
	if parent == nil {
		return nil, billingErrors.ErrPaymentNotFound
	}
	switch parent.Concept {
	case ConceptMembership, ConceptProduct:
	default:
		return nil, billingErrors.ErrCannotSettleConcept
	}
	if parent.BalancePending <= 0 {
		return nil, billingErrors.ErrNoBalancePending
	}
	if amount <= 0 {
		return nil, billingErrors.ErrAmountInvalid
	}
	if roundCents(amount) > roundCents(parent.BalancePending) {
		return nil, billingErrors.ErrSettlementExceedsBalance
	}
	if err := validateNotes(notes); err != nil {
		return nil, err
	}
	parentID := parent.ID
	return &Payment{
		ID:              id,
		GymID:           gymID,
		Version:         1,
		Folio:           folio,
		MemberID:        parent.MemberID,
		Amount:          roundCents(amount),
		PaymentMethod:   method,
		Concept:         ConceptBalanceSettlement,
		ParentPaymentID: &parentID,
		PaymentDate:     truncateDate(paymentDate),
		Notes:           notes,
		OperatorID:      operatorID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// NewProductSalePayment builds a Payment row for UC-025 (concept='product').
// `total` is the post-discount cart total (caller computed it from the Sale
// aggregate). `memberID` is optional (anonymous walk-in sale — DA-25.3).
func NewProductSalePayment(
	id, gymID, operatorID uuid.UUID,
	memberID *uuid.UUID,
	folio string,
	total float64,
	method string,
	paymentDate, now time.Time,
	notes *string,
) (*Payment, error) {
	if !validMethod(method) {
		if method == "" {
			return nil, billingErrors.ErrPaymentMethodMissing
		}
		return nil, billingErrors.ErrPaymentMethodInvalid
	}
	if total <= 0 {
		return nil, billingErrors.ErrAmountInvalid
	}
	if err := validateNotes(notes); err != nil {
		return nil, err
	}
	if paymentDate.IsZero() {
		return nil, billingErrors.ErrPaymentDateInvalid
	}
	return &Payment{
		ID:            id,
		GymID:         gymID,
		Version:       1,
		Folio:         folio,
		MemberID:      memberID,
		Amount:        roundCents(total),
		PaymentMethod: method,
		Concept:       ConceptProduct,
		PaymentDate:   truncateDate(paymentDate),
		Notes:         notes,
		OperatorID:    operatorID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

// NewRefundPayment builds a UC-022 refund row. `amount` is positive on input;
// it's stored as a negative number (DA-22.1). `reason` is mandatory.
func NewRefundPayment(
	id, gymID, operatorID uuid.UUID,
	parent *Payment,
	folio string,
	amount float64,
	method string,
	reason string,
	paymentDate, now time.Time,
) (*Payment, error) {
	if !validMethod(method) {
		if method == "" {
			return nil, billingErrors.ErrPaymentMethodMissing
		}
		return nil, billingErrors.ErrPaymentMethodInvalid
	}
	if parent == nil {
		return nil, billingErrors.ErrPaymentNotFound
	}
	switch parent.Concept {
	case ConceptMembership, ConceptProduct, ConceptOther:
	default:
		return nil, billingErrors.ErrCannotRefundNonPayment
	}
	if parent.Amount < 0 {
		return nil, billingErrors.ErrCannotRefundNonPayment
	}
	r := strings.TrimSpace(reason)
	if r == "" {
		return nil, billingErrors.ErrRefundReasonRequired
	}
	if amount <= 0 {
		return nil, billingErrors.ErrAmountInvalid
	}
	if roundCents(amount) > roundCents(parent.Amount) {
		return nil, billingErrors.ErrAmountInvalid
	}
	parentID := parent.ID
	notes := r
	return &Payment{
		ID:              id,
		GymID:           gymID,
		Version:         1,
		Folio:           folio,
		MemberID:        parent.MemberID,
		Amount:          -roundCents(amount),
		PaymentMethod:   method,
		Concept:         ConceptRefund,
		ParentPaymentID: &parentID,
		PaymentDate:     truncateDate(paymentDate),
		Notes:           &notes,
		OperatorID:      operatorID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// ApplyDiscount is exposed for tests / explicit construction flows. The use
// case for UC-018 calls NewMembershipPayment directly with the precomputed
// values; this helper is here to keep the domain self-contained.
func (p *Payment) ApplyDiscount(amount float64, reason string) error {
	if amount <= 0 {
		return billingErrors.ErrAmountInvalid
	}
	r := strings.TrimSpace(reason)
	if r == "" {
		return billingErrors.ErrDiscountReasonRequired
	}
	if amount >= p.Amount+p.DiscountAmount {
		return billingErrors.ErrDiscountTooLarge
	}
	p.DiscountAmount = roundCents(amount)
	p.DiscountReason = &r
	return nil
}

// RecordPartialPayment updates the mutable balance_pending. Used by UC-018 to
// flag a partial payment after the row was constructed (rare path) and by
// UC-019 to decrement on settlement.
func (p *Payment) RecordPartialPayment(paid, balance float64, now time.Time) error {
	if paid <= 0 || balance < 0 {
		return billingErrors.ErrPartialAmountInvalid
	}
	p.Amount = roundCents(paid)
	p.BalancePending = roundCents(balance)
	p.Version++
	p.UpdatedAt = now
	return nil
}

// DecrementBalance is the UC-019 update on the parent: subtract the settled
// amount, return the new pending balance. Returns an error if the settlement
// would exceed.
func (p *Payment) DecrementBalance(amount float64, now time.Time) (float64, error) {
	if amount <= 0 {
		return p.BalancePending, billingErrors.ErrAmountInvalid
	}
	if roundCents(amount) > roundCents(p.BalancePending) {
		return p.BalancePending, billingErrors.ErrSettlementExceedsBalance
	}
	p.BalancePending = roundCents(p.BalancePending - amount)
	p.Version++
	p.UpdatedAt = now
	return p.BalancePending, nil
}

// SetNotes mutates the (free-text) notes field. Bumps version. Used rarely.
func (p *Payment) SetNotes(notes *string, now time.Time) error {
	if err := validateNotes(notes); err != nil {
		return err
	}
	p.Notes = notes
	p.Version++
	p.UpdatedAt = now
	return nil
}

// Touch bumps version + UpdatedAt without changing any data. Used by the
// refund / settlement flows to force a re-enqueue of the parent payment in
// the sync queue right before persisting the dependent row — garantiza que
// el upsert del parent llegue al cloud antes que el FK del dependiente.
func (p *Payment) Touch(now time.Time) {
	p.Version++
	p.UpdatedAt = now
}

// HasMember is a convenience for receipts and listings.
func (p *Payment) HasMember() bool { return p.MemberID != nil }

// IsRefund reports whether this row is a refund.
func (p *Payment) IsRefund() bool { return p.Concept == ConceptRefund }

// IsRefundable returns true when a UC-022 refund row could legally point at
// this payment. The use case adds a "no double refund" check by querying
// existing rows.
func (p *Payment) IsRefundable() bool {
	switch p.Concept {
	case ConceptMembership, ConceptProduct, ConceptOther:
		return p.Amount > 0
	}
	return false
}

func validMethod(m string) bool {
	switch m {
	case MethodCash, MethodTransfer, MethodCard:
		return true
	}
	return false
}

func validateNotes(n *string) error {
	if n == nil {
		return nil
	}
	if len(*n) > 2000 {
		return billingErrors.ErrNotesTooLong
	}
	return nil
}

func truncateDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// roundCents rounds to 2 decimals to avoid float drift between JSON edges
// and the cents-stored SQLite mapper.
func roundCents(v float64) float64 {
	if v >= 0 {
		return float64(int64(v*100+0.5)) / 100
	}
	return float64(int64(v*100-0.5)) / 100
}
