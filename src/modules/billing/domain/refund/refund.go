package refund

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
)

const (
	ReturnedToStock = "returned_to_stock"
	Damaged         = "damaged"
	NotReturned     = "not_returned"

	KindRevenueRefund            = "revenue_refund"
	KindOvercollectionSettlement = "overcollection_settlement"
)

type Item struct {
	ID, GymID, RefundID, SaleItemID uuid.UUID
	Version                         int
	Quantity                        int
	Amount                          float64
	Disposition                     string
	CreatedAt, UpdatedAt            time.Time
	DeletedAt                       *time.Time
}

type Refund struct {
	ID, GymID, RootPaymentID, CreatedBy uuid.UUID
	RefundPaymentID                     *uuid.UUID
	Version                             int
	SaleID                              *uuid.UUID
	Amount                              float64
	Method                              string
	RefundedOn                          time.Time
	Reason                              string
	Kind                                string
	BalanceCancelled                    float64
	LegacyIncomplete                    bool
	CorrectionID                        *uuid.UUID
	IdempotencyKey                      string
	IdempotencyFingerprint              string
	IdempotencyResult                   json.RawMessage
	Items                               []*Item
	CreatedAt, UpdatedAt                time.Time
	DeletedAt                           *time.Time
}

type ItemInput struct {
	SaleItemID  uuid.UUID
	Quantity    int
	Amount      float64
	Disposition string
}

type Input struct {
	ID, GymID, RootPaymentID, CreatedBy uuid.UUID
	RefundPaymentID                     *uuid.UUID
	SaleID                              *uuid.UUID
	Amount                              float64
	Method                              string
	RefundedOn                          time.Time
	Reason                              string
	Kind                                string
	BalanceCancelled                    float64
	CorrectionID                        *uuid.UUID
	IdempotencyKey                      string
	IdempotencyFingerprint              string
	IdempotencyResult                   json.RawMessage
	Items                               []ItemInput
	Now                                 time.Time
}

func New(in Input) (*Refund, error) {
	key := strings.TrimSpace(in.IdempotencyKey)
	fingerprint := strings.TrimSpace(in.IdempotencyFingerprint)
	reason := strings.TrimSpace(in.Reason)
	if in.ID == uuid.Nil || in.GymID == uuid.Nil || in.RootPaymentID == uuid.Nil ||
		in.CreatedBy == uuid.Nil || in.RefundedOn.IsZero() {
		return nil, billingErrors.ErrAmountInvalid
	}
	if key == "" || len(key) > 120 {
		return nil, billingErrors.ErrRefundIdempotencyRequired
	}
	if fingerprint == "" || !json.Valid(in.IdempotencyResult) {
		return nil, billingErrors.ErrRefundIdempotencyRequired
	}
	if reason == "" || len(reason) > 200 {
		return nil, billingErrors.ErrRefundReasonRequired
	}
	if !validNonnegativeMoney(in.Amount) || !validNonnegativeMoney(in.BalanceCancelled) ||
		(in.Amount == 0 && in.BalanceCancelled == 0) ||
		(in.Amount > 0 && (in.RefundPaymentID == nil || *in.RefundPaymentID == uuid.Nil)) ||
		(in.Amount == 0 && in.RefundPaymentID != nil) {
		return nil, billingErrors.ErrAmountInvalid
	}
	if (in.Amount > 0 && in.Method != "cash" && in.Method != "transfer" && in.Method != "card") ||
		(in.Amount == 0 && strings.TrimSpace(in.Method) != "") {
		return nil, billingErrors.ErrPaymentMethodInvalid
	}
	r := &Refund{
		ID: in.ID, GymID: in.GymID, Version: 1, RootPaymentID: in.RootPaymentID,
		RefundPaymentID: in.RefundPaymentID, SaleID: in.SaleID,
		Amount: round(in.Amount), Method: in.Method,
		RefundedOn: dateOnly(in.RefundedOn), Reason: reason,
		BalanceCancelled: round(in.BalanceCancelled), CorrectionID: in.CorrectionID,
		IdempotencyKey: key, IdempotencyFingerprint: fingerprint,
		IdempotencyResult: append(json.RawMessage(nil), in.IdempotencyResult...), CreatedBy: in.CreatedBy,
		CreatedAt: in.Now.UTC(), UpdatedAt: in.Now.UTC(),
	}
	if in.Kind == "" {
		in.Kind = KindRevenueRefund
	}
	if in.Kind != KindRevenueRefund && in.Kind != KindOvercollectionSettlement {
		return nil, billingErrors.ErrAmountInvalid
	}
	r.Kind = in.Kind
	if r.Kind == KindOvercollectionSettlement && (r.CorrectionID == nil || r.Amount <= 0 || r.BalanceCancelled != 0) {
		return nil, billingErrors.ErrAmountInvalid
	}
	var itemTotal float64
	for _, line := range in.Items {
		if line.SaleItemID == uuid.Nil || line.Quantity <= 0 || !validMoney(line.Amount) || !validDisposition(line.Disposition) {
			return nil, billingErrors.ErrRefundDispositionInvalid
		}
		item := &Item{
			ID: uuid.New(), GymID: in.GymID, RefundID: in.ID, SaleItemID: line.SaleItemID,
			Version: 1, Quantity: line.Quantity, Amount: round(line.Amount),
			Disposition: line.Disposition, CreatedAt: in.Now.UTC(), UpdatedAt: in.Now.UTC(),
		}
		r.Items = append(r.Items, item)
		itemTotal += item.Amount
	}
	if len(r.Items) > 0 && round(itemTotal) != round(r.Amount+r.BalanceCancelled) {
		return nil, billingErrors.ErrAmountInvalid
	}
	return r, nil
}

func validDisposition(v string) bool {
	return v == ReturnedToStock || v == Damaged || v == NotReturned
}

func validMoney(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v*100-math.Round(v*100)) <= 1e-7
}
func validNonnegativeMoney(v float64) bool {
	return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v*100-math.Round(v*100)) <= 1e-7
}
func round(v float64) float64 { return math.Round(v*100) / 100 }
func dateOnly(v time.Time) time.Time {
	return time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC)
}
