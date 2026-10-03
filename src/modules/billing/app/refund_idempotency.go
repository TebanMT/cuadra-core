package app

import (
	"strings"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type refundFingerprintItem struct {
	SaleItemID  uuid.UUID `json:"sale_item_id"`
	Quantity    int       `json:"quantity"`
	Disposition string    `json:"disposition"`
}

type refundFingerprintPayload struct {
	GymID            uuid.UUID               `json:"gym_id"`
	ActorUserID      uuid.UUID               `json:"actor_user_id"`
	ParentPaymentID  uuid.UUID               `json:"parent_payment_id"`
	Reason           string                  `json:"reason"`
	Method           string                  `json:"method"`
	CashDrawerID     *uuid.UUID              `json:"cash_drawer_id"`
	AmountCents      int64                   `json:"amount_cents"`
	PaymentDate      string                  `json:"payment_date"`
	RevertMembership bool                    `json:"revert_membership"`
	SaleID           *uuid.UUID              `json:"sale_id"`
	Items            []refundFingerprintItem `json:"items"`
	CorrectionID     *uuid.UUID              `json:"correction_id"`
	Kind             string                  `json:"kind"`
	MonetaryCents    int64                   `json:"monetary_cents"`
	CancelledCents   int64                   `json:"cancelled_cents"`
}

// refundCommandFingerprint intentionally excludes client-supplied item
// amounts: product return money is allocated authoritatively by the server.
// Item order is normalized because JSON array order is not business meaning.
func refundCommandFingerprint(in RefundPaymentInput) (string, error) {
	items := make([]refundFingerprintItem, 0, len(in.Items))
	for _, item := range in.Items {
		items = append(items, refundFingerprintItem{
			SaleItemID: item.SaleItemID, Quantity: item.Quantity,
			Disposition: strings.TrimSpace(item.Disposition),
		})
	}
	sortRefundFingerprintItems(items)
	day := ""
	if !in.PaymentDate.IsZero() {
		day = in.PaymentDate.Format("2006-01-02")
	}
	return paymentCommandFingerprint(refundFingerprintPayload{
		GymID: in.GymID, ActorUserID: in.ActorUserID, ParentPaymentID: in.ParentPaymentID,
		Reason: strings.TrimSpace(in.Reason), Method: strings.TrimSpace(in.Method),
		CashDrawerID: in.CashDrawerID, AmountCents: int64(cents(in.Amount)), PaymentDate: day,
		RevertMembership: in.RevertMembership, SaleID: in.SaleID, Items: items,
		CorrectionID: in.CorrectionID, Kind: strings.TrimSpace(in.Kind),
		MonetaryCents: int64(cents(in.MonetaryAmount)), CancelledCents: int64(cents(in.BalanceCancelled)),
	})
}

func sortRefundFingerprintItems(items []refundFingerprintItem) {
	// Insertion sort is sufficient for a receipt-sized cart and keeps this
	// helper independent of the domain Item type.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && refundFingerprintItemLess(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func refundFingerprintItemLess(a, b refundFingerprintItem) bool {
	if a.SaleItemID != b.SaleItemID {
		return a.SaleItemID.String() < b.SaleItemID.String()
	}
	if a.Quantity != b.Quantity {
		return a.Quantity < b.Quantity
	}
	return a.Disposition < b.Disposition
}

func validateRefundCommandKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		// HTTP requires a key. This compatibility key is only for old internal
		// callers and intentionally cannot deduplicate a later invocation.
		return "legacy:" + uuid.NewString(), nil
	}
	if len(key) > 120 {
		return "", sharedDomain.NewValidationError(billingErrors.ErrRefundIdempotencyRequired)
	}
	return key, nil
}
