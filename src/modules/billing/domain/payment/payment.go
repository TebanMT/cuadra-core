// Package payment holds the Payment aggregate. Append-only by convention:
// the only mutable fields after creation are `notes` and `balance_pending`
// (the latter is decremented by UC-019 settlements). Refunds and settlements
// are NEW rows that point at the parent via parent_payment_id (see ADR-002
// §3.9 and UC-018..UC-022 for the full model).
package payment

import (
	"encoding/json"
	"math"
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
	ID       uuid.UUID
	GymID    uuid.UUID
	Version  int
	Folio    string
	MemberID *uuid.UUID
	// MembershipID identifies the exact service obligation created/activated
	// by a membership payment. It is intentionally separate from MemberID so
	// refunding an old payment can never cancel a later renewal.
	MembershipID *uuid.UUID
	// Keyed financial commands persist their request fingerprint and original
	// response on the payment they create, so retries never repeat side effects.
	IdempotencyKey         string
	IdempotencyFingerprint string
	IdempotencyResult      json.RawMessage
	Amount                 float64
	// RecognizedAmount is the economic income attributed to this collection.
	// It normally equals Amount; a corrected overcollection keeps physical
	// Amount intact while recognizing only the real sale total.
	RecognizedAmount float64
	PaymentMethod    string
	// CashDrawerID attributes a physical cash event to one drawer. Nil means
	// the deterministic main drawer and remains valid for older clients.
	CashDrawerID    *uuid.UUID
	CashDestination string
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

func (p *Payment) WithCashDrawer(drawerID uuid.UUID) *Payment {
	if p != nil && p.PaymentMethod == MethodCash && p.EffectiveCashDestination() != "gym_fund" && drawerID != uuid.Nil {
		id := drawerID
		p.CashDrawerID = &id
	}
	return p
}

func (p *Payment) WithMembership(membershipID uuid.UUID) *Payment {
	if p != nil && membershipID != uuid.Nil && (p.Concept == ConceptMembership || p.Concept == ConceptBalanceSettlement) {
		id := membershipID
		p.MembershipID = &id
	}
	return p
}

// WithProductSaleDiscount mirrors Sale.Discount onto the payment read model
// so receipts and payment exports do not report a discounted sale as if it
// had no discount. Product discounts may be manual and therefore need not
// have a reason.
func (p *Payment) WithProductSaleDiscount(amount float64, reason *string) *Payment {
	if p == nil || p.Concept != ConceptProduct || amount < 0 {
		return p
	}
	p.DiscountAmount = roundCents(amount)
	if reason != nil && strings.TrimSpace(*reason) != "" {
		clean := strings.TrimSpace(*reason)
		p.DiscountReason = &clean
	}
	return p
}

func (p *Payment) BeginIdempotency(key, fingerprint string) error {
	key, fingerprint = strings.TrimSpace(key), strings.TrimSpace(fingerprint)
	if p == nil || key == "" || len(key) > 120 || fingerprint == "" {
		return billingErrors.ErrIdempotencyKeyRequired
	}
	p.IdempotencyKey = key
	p.IdempotencyFingerprint = fingerprint
	return nil
}

func (p *Payment) FinalizeIdempotency(result []byte, now time.Time) error {
	if p == nil || p.IdempotencyKey == "" || p.IdempotencyFingerprint == "" || !json.Valid(result) {
		return billingErrors.ErrIdempotencyKeyRequired
	}
	p.IdempotencyResult = append(json.RawMessage(nil), result...)
	p.Version++
	p.UpdatedAt = now.UTC()
	return nil
}

// EffectiveCashDrawerID normalizes legacy/nil cash attribution to the
// deterministic main drawer. Non-cash payments have no physical drawer.
func (p *Payment) EffectiveCashDrawerID() uuid.UUID {
	if p == nil || p.PaymentMethod != MethodCash || p.EffectiveCashDestination() == "gym_fund" {
		return uuid.Nil
	}
	if p.CashDrawerID != nil && *p.CashDrawerID != uuid.Nil {
		return *p.CashDrawerID
	}
	return p.GymID
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
		ID:               id,
		GymID:            gymID,
		Version:          1,
		Folio:            folio,
		MemberID:         &mID,
		Amount:           roundCents(paid),
		RecognizedAmount: roundCents(paid),
		PaymentMethod:    method,
		Concept:          ConceptMembership,
		DiscountAmount:   roundCents(discount),
		DiscountReason:   discountReason,
		BalancePending:   roundCents(balancePending),
		PaymentDate:      truncateDate(paymentDate),
		Notes:            notes,
		OperatorID:       operatorID,
		CreatedAt:        now,
		UpdatedAt:        now,
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
		ID:               id,
		GymID:            gymID,
		Version:          1,
		Folio:            folio,
		MemberID:         parent.MemberID,
		MembershipID:     parent.MembershipID,
		Amount:           roundCents(amount),
		RecognizedAmount: roundCents(amount),
		PaymentMethod:    method,
		Concept:          ConceptBalanceSettlement,
		ParentPaymentID:  &parentID,
		PaymentDate:      truncateDate(paymentDate),
		Notes:            notes,
		OperatorID:       operatorID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

// NewProductSalePayment builds a Payment row for UC-025 (concept='product').
// `total` is the post-discount cart total (caller computed it from the Sale
// aggregate). `memberID` is optional (anonymous walk-in sale — DA-25.3).
// `paid` puede ser igual al total (cobro completo, caso default) o menor
// (fiado — se cobra una parte o nada y el resto queda como balance_pending para
// liquidar después vía POST /payments/:id/settle, el mismo flujo que los
// abonos a mensualidades). El fiado requiere un socio; cero cobrado
// conserva la deuda sin registrar ingresos.
func NewProductSalePayment(
	id, gymID, operatorID uuid.UUID,
	memberID *uuid.UUID,
	folio string,
	total, paid float64,
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
	if math.IsNaN(total) || math.IsInf(total, 0) || total <= 0 {
		return nil, billingErrors.ErrAmountInvalid
	}
	if math.IsNaN(paid) || math.IsInf(paid, 0) || paid < 0 || roundCents(paid) > roundCents(total) {
		return nil, billingErrors.ErrSalePaidInvalid
	}
	if roundCents(paid) < roundCents(total) && memberID == nil {
		return nil, billingErrors.ErrCreditRequiresMember
	}
	if err := validateNotes(notes); err != nil {
		return nil, err
	}
	if paymentDate.IsZero() {
		return nil, billingErrors.ErrPaymentDateInvalid
	}
	balance := roundCents(total - paid)
	return &Payment{
		ID:               id,
		GymID:            gymID,
		Version:          1,
		Folio:            folio,
		MemberID:         memberID,
		Amount:           roundCents(paid),
		RecognizedAmount: roundCents(paid),
		PaymentMethod:    method,
		Concept:          ConceptProduct,
		BalancePending:   balance,
		PaymentDate:      truncateDate(paymentDate),
		Notes:            notes,
		OperatorID:       operatorID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

// NewOtherIncomePayment records income that is not a membership charge or a
// product sale (for example, selling old equipment). The description lives in
// Notes because payments are append-only and this concept does not need a
// separate aggregate.
func NewOtherIncomePayment(
	id, gymID, operatorID uuid.UUID,
	folio string,
	amount float64,
	method string,
	description string,
	paymentDate, now time.Time,
) (*Payment, error) {
	if !validMethod(method) {
		if method == "" {
			return nil, billingErrors.ErrPaymentMethodMissing
		}
		return nil, billingErrors.ErrPaymentMethodInvalid
	}
	if amount <= 0 {
		return nil, billingErrors.ErrAmountInvalid
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return nil, billingErrors.ErrOtherIncomeDescriptionRequired
	}
	if err := validateNotes(&description); err != nil {
		return nil, err
	}
	if paymentDate.IsZero() {
		return nil, billingErrors.ErrPaymentDateInvalid
	}
	return &Payment{
		ID: id, GymID: gymID, Version: 1, Folio: folio,
		Amount: roundCents(amount), RecognizedAmount: roundCents(amount), PaymentMethod: method, Concept: ConceptOther,
		PaymentDate: truncateDate(paymentDate), Notes: &description,
		OperatorID: operatorID, CreatedAt: now, UpdatedAt: now,
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
	return NewRefundPaymentWithin(id, gymID, operatorID, parent, folio, amount,
		parentRefundLimit(parent), method, reason, paymentDate, now)
}

// NewRefundPaymentWithin validates against the aggregate refundable balance
// supplied by a repository lock (root + settlements - previous refunds).
func NewRefundPaymentWithin(
	id, gymID, operatorID uuid.UUID,
	parent *Payment,
	folio string,
	amount, refundable float64,
	method string,
	reason string,
	paymentDate, now time.Time,
) (*Payment, error) {
	if !validRefundMethod(method) {
		if method == "" {
			return nil, billingErrors.ErrPaymentMethodMissing
		}
		return nil, billingErrors.ErrPaymentMethodInvalid
	}
	if parent == nil {
		return nil, billingErrors.ErrPaymentNotFound
	}
	switch parent.Concept {
	case ConceptMembership, ConceptProduct, ConceptOther, ConceptBalanceSettlement:
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
	if roundCents(amount) > roundCents(refundable) {
		return nil, billingErrors.ErrRefundExceedsCollected
	}
	parentID := parent.ID
	notes := r
	return &Payment{
		ID:               id,
		GymID:            gymID,
		Version:          1,
		Folio:            folio,
		MemberID:         parent.MemberID,
		MembershipID:     parent.MembershipID,
		Amount:           -roundCents(amount),
		RecognizedAmount: 0,
		PaymentMethod:    method,
		Concept:          ConceptRefund,
		ParentPaymentID:  &parentID,
		PaymentDate:      truncateDate(paymentDate),
		Notes:            &notes,
		OperatorID:       operatorID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

func parentRefundLimit(parent *Payment) float64 {
	if parent == nil {
		return 0
	}
	return parent.Amount
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
	p.RecognizedAmount = roundCents(paid)
	p.BalancePending = roundCents(balance)
	p.Version++
	p.UpdatedAt = now
	return nil
}

// CorrectSaleAmounts is the only domain mutation that may rewrite Amount.
// It is intentionally called only by the audited sale-correction use case;
// ordinary payment updates remain append-only.
func (p *Payment) CorrectSaleAmounts(physicalAmount, recognizedAmount, balancePending, discountAmount float64, breakdown []BreakdownLine, now time.Time) error {
	if p.Concept != ConceptProduct || physicalAmount < 0 || recognizedAmount < 0 ||
		recognizedAmount > physicalAmount || balancePending < 0 || discountAmount < 0 {
		return billingErrors.ErrAmountInvalid
	}
	p.Amount = roundCents(physicalAmount)
	p.RecognizedAmount = roundCents(recognizedAmount)
	p.BalancePending = roundCents(balancePending)
	p.DiscountAmount = roundCents(discountAmount)
	p.Breakdown = append([]BreakdownLine(nil), breakdown...)
	p.Version++
	p.UpdatedAt = now.UTC()
	return nil
}

// CorrectAdministrative rewrites capture facts for membership/other income
// only. It does not alter the membership service period; product payments,
// settlements and refunds have dedicated flows with additional consequences.
func (p *Payment) CorrectAdministrative(amount, balancePending float64, method string, cashDrawerID *uuid.UUID, paymentDate, now time.Time) error {
	if p == nil || p.ParentPaymentID != nil || (p.Concept != ConceptMembership && p.Concept != ConceptOther) ||
		amount <= 0 || balancePending < 0 || !validMethod(method) || paymentDate.IsZero() {
		return billingErrors.ErrPaymentCorrectionUnsupported
	}
	if p.Concept == ConceptOther && balancePending != 0 {
		return billingErrors.ErrPaymentCorrectionUnsupported
	}
	if method != MethodCash || p.EffectiveCashDestination() == "gym_fund" {
		cashDrawerID = nil
	} else if cashDrawerID == nil || *cashDrawerID == uuid.Nil {
		main := p.GymID
		cashDrawerID = &main
	} else {
		copyID := *cashDrawerID
		cashDrawerID = &copyID
	}
	p.Amount = roundCents(amount)
	p.RecognizedAmount = roundCents(amount)
	p.BalancePending = roundCents(balancePending)
	p.PaymentMethod = method
	p.CashDrawerID = cashDrawerID
	p.PaymentDate = truncateDate(paymentDate)
	p.Version++
	p.UpdatedAt = now.UTC()
	return nil
}

// AnnulAdministrative tombstones an extraordinary-income capture that never
// happened. It deliberately does not create a refund or zero the historical
// amount: reports ignore the tombstone while the correction snapshot retains
// the exact erroneous capture. Membership payments are excluded because
// removing one safely also requires reverting its service effects.
func (p *Payment) AnnulAdministrative(now time.Time) error {
	if p == nil || p.ParentPaymentID != nil || p.Concept != ConceptOther || p.DeletedAt != nil {
		return billingErrors.ErrPaymentCorrectionUnsupported
	}
	at := now.UTC()
	p.DeletedAt = &at
	p.Version++
	p.UpdatedAt = at
	return nil
}

// CancelBalance removes the uncollected portion of an obligation without
// creating a cash event. Product returns apply their economic value to this
// balance first; only any excess becomes money physically refunded.
func (p *Payment) CancelBalance(amount float64, now time.Time) (float64, error) {
	if amount <= 0 || roundCents(amount) > roundCents(p.BalancePending) {
		return p.BalancePending, billingErrors.ErrAmountInvalid
	}
	p.BalancePending = roundCents(p.BalancePending - amount)
	p.Version++
	p.UpdatedAt = now.UTC()
	return p.BalancePending, nil
}

// ReopenBalance reverses a collection previously applied by a child
// balance_settlement. Refunding that child returns physical money and makes
// exactly the same amount collectible again on the original obligation.
func (p *Payment) ReopenBalance(amount float64, now time.Time) (float64, error) {
	if p == nil || (p.Concept != ConceptMembership && p.Concept != ConceptProduct) || amount <= 0 {
		if p == nil {
			return 0, billingErrors.ErrAmountInvalid
		}
		return p.BalancePending, billingErrors.ErrAmountInvalid
	}
	p.BalancePending = roundCents(p.BalancePending + amount)
	p.Version++
	p.UpdatedAt = now.UTC()
	return p.BalancePending, nil
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
	case ConceptProduct:
		// The root may collect zero; returns can cancel its debt or refund later settlements.
		return p.Amount >= 0
	case ConceptMembership, ConceptOther, ConceptBalanceSettlement:
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

func validRefundMethod(m string) bool {
	return validMethod(m)
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

// EffectiveCashDestination preserves the physical attribution of older clients.
func (p *Payment) EffectiveCashDestination() string {
	if p != nil && p.CashDestination == "gym_fund" {
		return "gym_fund"
	}
	return "cash_drawer"
}
func (p *Payment) SetCashDestination(destination string) error {
	if destination == "" {
		destination = "cash_drawer"
	}
	if destination != "cash_drawer" && destination != "gym_fund" {
		return billingErrors.ErrPaymentCorrectionUnsupported
	}
	if destination == "gym_fund" && p.Concept != ConceptOther {
		return billingErrors.ErrPaymentCorrectionUnsupported
	}
	p.CashDestination = destination
	if destination == "gym_fund" {
		p.CashDrawerID = nil
	}
	return nil
}
