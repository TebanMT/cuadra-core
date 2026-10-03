//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// PaymentModel mirrors `payments` (ADR-002 §3.9). The domain entity stays
// free of GORM tags; the mapper in repositories/ bridges them.
type PaymentModel struct {
	ID                     uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID                  uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version                int        `gorm:"not null;default:1;column:version"`
	CreatedAt              time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt              time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt              *time.Time `gorm:"column:deleted_at"`
	Folio                  string     `gorm:"not null;column:folio"`
	MemberID               *uuid.UUID `gorm:"type:uuid;column:member_id"`
	MembershipID           *uuid.UUID `gorm:"type:uuid;column:membership_id"`
	IdempotencyKey         *string    `gorm:"column:idempotency_key"`
	IdempotencyFingerprint *string    `gorm:"column:idempotency_fingerprint"`
	IdempotencyResult      *string    `gorm:"type:jsonb;column:idempotency_result"`
	Amount                 float64    `gorm:"type:numeric(12,2);not null;column:amount"`
	RecognizedAmount       float64    `gorm:"type:numeric(12,2);not null;default:0;column:recognized_amount"`
	PaymentMethod          string     `gorm:"not null;column:payment_method"`
	CashDestination        string     `gorm:"column:cash_destination;not null;default:cash_drawer"`
	CashDrawerID           *uuid.UUID `gorm:"type:uuid;column:cash_drawer_id"`
	Concept                string     `gorm:"not null;column:concept"`
	ParentPaymentID        *uuid.UUID `gorm:"type:uuid;column:parent_payment_id"`
	DiscountAmount         float64    `gorm:"type:numeric(12,2);not null;default:0;column:discount_amount"`
	DiscountReason         *string    `gorm:"column:discount_reason"`
	BalancePending         float64    `gorm:"type:numeric(12,2);not null;default:0;column:balance_pending"`
	PaymentDate            time.Time  `gorm:"type:date;not null;column:payment_date"`
	Notes                  *string    `gorm:"column:notes"`
	// Breakdown is stored as JSONB on Postgres. Held as a raw string at
	// the model layer; mapper en repositories/ marshala/desmarshala.
	Breakdown  *string   `gorm:"type:jsonb;column:breakdown"`
	OperatorID uuid.UUID `gorm:"type:uuid;not null;column:operator_id"`
}

func (PaymentModel) TableName() string { return "payments" }
