package promotion

import (
	"time"

	"github.com/google/uuid"
)

// AppliedPromotion es la entity inmutable que registra una aplicación
// de promo a un cobro específico. Snapshot fields (name, kind, value)
// permiten que la Promotion subyacente cambie sin afectar este registro.
//
// MemberID es opcional porque las promos applies_to=sale pueden tocar
// ventas a walk-ins sin socio asociado.
type AppliedPromotion struct {
	ID                    uuid.UUID
	GymID                 uuid.UUID
	Version               int
	PromotionID           uuid.UUID
	PaymentID             uuid.UUID
	MemberID              *uuid.UUID
	AppliedByUserID       uuid.UUID
	PromotionNameSnapshot string
	KindSnapshot          string
	ValueSnapshot         *float64
	DiscountAmount        float64
	ExtraDaysApplied      int
	Notes                 *string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	DeletedAt             *time.Time
}

// NewAppliedParams agrupa el input.
type NewAppliedParams struct {
	PromotionID     uuid.UUID
	PaymentID       uuid.UUID
	MemberID        *uuid.UUID
	AppliedByUserID uuid.UUID
	Notes           *string
}

// NewApplied construye un AppliedPromotion tomando la promo + el efecto
// resuelto del Calculator. Snapshot fields se llenan en el momento.
func NewApplied(id, gymID uuid.UUID, p *Promotion, result CalcResult, params NewAppliedParams, now time.Time) *AppliedPromotion {
	ap := &AppliedPromotion{
		ID:                    id,
		GymID:                 gymID,
		Version:               1,
		PromotionID:           params.PromotionID,
		PaymentID:             params.PaymentID,
		MemberID:              params.MemberID,
		AppliedByUserID:       params.AppliedByUserID,
		PromotionNameSnapshot: p.Name,
		KindSnapshot:          p.Kind,
		DiscountAmount:        result.Discount,
		ExtraDaysApplied:      result.ExtraDays,
		Notes:                 params.Notes,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if p.Value != nil {
		v := *p.Value
		ap.ValueSnapshot = &v
	}
	return ap
}
