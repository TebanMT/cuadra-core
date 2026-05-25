//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// PromotionModel mirrors `promotions` (migration 028 / ADR-002 §3).
type PromotionModel struct {
	ID               uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID            uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version          int        `gorm:"not null;default:1;column:version"`
	CreatedAt        time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt        time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt        *time.Time `gorm:"column:deleted_at"`
	Name             string     `gorm:"not null;column:name"`
	Description      *string    `gorm:"column:description"`
	Kind             string     `gorm:"not null;column:kind"`
	Value            *float64   `gorm:"type:numeric(12,2);column:value"`
	BuyN             int        `gorm:"not null;default:1;column:buy_n"`
	CompanionCount   *int       `gorm:"column:companion_count"`
	AppliesTo        string     `gorm:"not null;column:applies_to"`
	Code             *string    `gorm:"column:code"`
	ValidFrom        *time.Time `gorm:"type:date;column:valid_from"`
	ValidUntil       *time.Time `gorm:"type:date;column:valid_until"`
	MaxUsesTotal     *int       `gorm:"column:max_uses_total"`
	MaxUsesPerMember *int       `gorm:"column:max_uses_per_member"`
	Active           bool       `gorm:"not null;default:true;column:active"`
}

func (PromotionModel) TableName() string { return "promotions" }

// AppliedPromotionModel mirrors `applied_promotions` (migration 028).
type AppliedPromotionModel struct {
	ID                    uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID                 uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version               int        `gorm:"not null;default:1;column:version"`
	CreatedAt             time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt             time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt             *time.Time `gorm:"column:deleted_at"`
	PromotionID           uuid.UUID  `gorm:"type:uuid;not null;column:promotion_id"`
	PaymentID             uuid.UUID  `gorm:"type:uuid;not null;column:payment_id"`
	MemberID              *uuid.UUID `gorm:"type:uuid;column:member_id"`
	AppliedByUserID       uuid.UUID  `gorm:"type:uuid;not null;column:applied_by_user_id"`
	PromotionNameSnapshot string     `gorm:"not null;column:promotion_name_snapshot"`
	KindSnapshot          string     `gorm:"not null;column:kind_snapshot"`
	ValueSnapshot         *float64   `gorm:"type:numeric(12,2);column:value_snapshot"`
	DiscountAmount        float64    `gorm:"type:numeric(12,2);not null;default:0;column:discount_amount"`
	ExtraDaysApplied      int        `gorm:"not null;default:0;column:extra_days_applied"`
	Notes                 *string    `gorm:"column:notes"`
}

func (AppliedPromotionModel) TableName() string { return "applied_promotions" }
