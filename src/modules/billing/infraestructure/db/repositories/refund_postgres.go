//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	refundDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/refund"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type refundPG struct {
	ID, GymID, RootPaymentID, CreatedBy uuid.UUID
	RefundPaymentID                     *uuid.UUID
	Version                             int
	CreatedAt, UpdatedAt                time.Time
	DeletedAt                           *time.Time
	SaleID                              *uuid.UUID
	Amount                              float64
	Method                              *string
	RefundedOn                          time.Time `gorm:"type:date"`
	Reason                              string
	Kind                                string
	BalanceCancelled                    float64
	LegacyIncomplete                    bool
	CorrectionID                        *uuid.UUID
	IdempotencyKey                      string
	IdempotencyFingerprint              string
	IdempotencyResult                   json.RawMessage `gorm:"type:jsonb"`
}

func (refundPG) TableName() string { return "refunds" }

type refundItemPG struct {
	ID, GymID, RefundID, SaleItemID uuid.UUID
	Version                         int
	CreatedAt, UpdatedAt            time.Time
	DeletedAt                       *time.Time
	Quantity                        int
	Amount                          float64
	Disposition                     string
}

func (refundItemPG) TableName() string { return "refund_items" }

type RefundPostgresRepository struct{}

func NewRefundPostgresRepository() *RefundPostgresRepository { return &RefundPostgresRepository{} }

func (r *RefundPostgresRepository) Create(tx sharedDomain.Transaction, aggregate *refundDomain.Refund) (*refundDomain.Refund, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	row := refundToPG(aggregate)
	if err := g.Create(&row).Error; err != nil {
		return nil, err
	}
	if len(aggregate.Items) > 0 {
		items := make([]refundItemPG, len(aggregate.Items))
		for i, item := range aggregate.Items {
			items[i] = refundItemToPG(item)
		}
		if err := g.Create(&items).Error; err != nil {
			return nil, err
		}
	}
	if err := mirrorRefundAggregate(g, aggregate); err != nil {
		return nil, err
	}
	return aggregate, nil
}

func (r *RefundPostgresRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*refundDomain.Refund, error) {
	var row refundPG
	err := tx.(*sharedDomain.GormTransaction).Tx.
		Where("gym_id=? AND idempotency_key=? AND deleted_at IS NULL", gymID, key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return refundFromPG(row), nil
}

func (r *RefundPostgresRepository) RefundedQuantitiesBySale(tx sharedDomain.Transaction, gymID, saleID uuid.UUID) (map[uuid.UUID]int, error) {
	type row struct {
		SaleItemID uuid.UUID
		Quantity   int
	}
	var rows []row
	err := tx.(*sharedDomain.GormTransaction).Tx.Raw(`
		SELECT ri.sale_item_id, SUM(ri.quantity)::int AS quantity
		FROM refund_items ri
		JOIN refunds r ON r.id=ri.refund_id AND r.deleted_at IS NULL
		WHERE ri.gym_id=? AND r.sale_id=? AND ri.deleted_at IS NULL
		GROUP BY ri.sale_item_id`, gymID, saleID).Scan(&rows).Error
	out := make(map[uuid.UUID]int, len(rows))
	for _, x := range rows {
		out[x.SaleItemID] = x.Quantity
	}
	return out, err
}

func refundToPG(r *refundDomain.Refund) refundPG {
	var method *string
	if r.Method != "" {
		value := r.Method
		method = &value
	}
	return refundPG{
		ID: r.ID, GymID: r.GymID, RootPaymentID: r.RootPaymentID,
		RefundPaymentID: r.RefundPaymentID, CreatedBy: r.CreatedBy,
		Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		DeletedAt: r.DeletedAt, SaleID: r.SaleID, Amount: r.Amount,
		Method: method, RefundedOn: r.RefundedOn, Reason: r.Reason,
		Kind:             r.Kind,
		BalanceCancelled: r.BalanceCancelled, LegacyIncomplete: r.LegacyIncomplete,
		CorrectionID: r.CorrectionID, IdempotencyKey: r.IdempotencyKey,
		IdempotencyFingerprint: r.IdempotencyFingerprint,
		IdempotencyResult:      append(json.RawMessage(nil), r.IdempotencyResult...),
	}
}

func refundFromPG(x refundPG) *refundDomain.Refund {
	method := ""
	if x.Method != nil {
		method = *x.Method
	}
	return &refundDomain.Refund{
		ID: x.ID, GymID: x.GymID, RootPaymentID: x.RootPaymentID,
		RefundPaymentID: x.RefundPaymentID, CreatedBy: x.CreatedBy,
		Version: x.Version, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt,
		DeletedAt: x.DeletedAt, SaleID: x.SaleID, Amount: x.Amount,
		Method: method, RefundedOn: x.RefundedOn, Reason: x.Reason,
		Kind:             x.Kind,
		BalanceCancelled: x.BalanceCancelled, LegacyIncomplete: x.LegacyIncomplete,
		CorrectionID: x.CorrectionID, IdempotencyKey: x.IdempotencyKey,
		IdempotencyFingerprint: x.IdempotencyFingerprint,
		IdempotencyResult:      append(json.RawMessage(nil), x.IdempotencyResult...),
	}
}

func refundItemToPG(x *refundDomain.Item) refundItemPG {
	return refundItemPG{
		ID: x.ID, GymID: x.GymID, RefundID: x.RefundID, SaleItemID: x.SaleItemID,
		Version: x.Version, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt,
		DeletedAt: x.DeletedAt, Quantity: x.Quantity, Amount: x.Amount,
		Disposition: x.Disposition,
	}
}

func mirrorRefundAggregate(g *gorm.DB, r *refundDomain.Refund) error {
	payload, err := json.Marshal(map[string]any{
		"id": r.ID, "gym_id": r.GymID, "version": r.Version,
		"created_at": r.CreatedAt.UnixMilli(), "updated_at": r.UpdatedAt.UnixMilli(), "deleted_at": r.DeletedAt,
		"root_payment_id": r.RootPaymentID, "refund_payment_id": r.RefundPaymentID, "sale_id": r.SaleID,
		"amount": r.Amount, "method": r.Method, "refunded_on": r.RefundedOn.Format("2006-01-02"),
		"reason": r.Reason, "kind": r.Kind, "balance_cancelled": r.BalanceCancelled,
		"legacy_incomplete": r.LegacyIncomplete, "correction_id": r.CorrectionID,
		"idempotency_key": r.IdempotencyKey, "idempotency_fingerprint": r.IdempotencyFingerprint,
		"idempotency_result": json.RawMessage(r.IdempotencyResult), "created_by": r.CreatedBy,
	})
	if err != nil {
		return err
	}
	if err := upsertFinancialMirror(g, r.GymID, "refunds", r.ID, r.Version, payload, r.DeletedAt); err != nil {
		return err
	}
	for _, item := range r.Items {
		payload, err := json.Marshal(map[string]any{
			"id": item.ID, "gym_id": item.GymID, "version": item.Version,
			"created_at": item.CreatedAt.UnixMilli(), "updated_at": item.UpdatedAt.UnixMilli(), "deleted_at": item.DeletedAt,
			"refund_id": item.RefundID, "sale_item_id": item.SaleItemID,
			"quantity": item.Quantity, "amount": item.Amount, "disposition": item.Disposition,
		})
		if err != nil {
			return err
		}
		if err := upsertFinancialMirror(g, item.GymID, "refund_items", item.ID, item.Version, payload, item.DeletedAt); err != nil {
			return err
		}
	}
	return nil
}
