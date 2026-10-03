//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// CashCloseEventModel mirrors `cash_close_events` (ADR-002 §3.14).
// `Discrepancy` is a generated column — read-only on this side.
type CashCloseEventModel struct {
	ID                      uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID                   uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version                 int        `gorm:"not null;default:1;column:version"`
	CreatedAt               time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt               time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt               *time.Time `gorm:"column:deleted_at"`
	DrawerID                uuid.UUID  `gorm:"type:uuid;not null;column:drawer_id"`
	DrawerCode              string     `gorm:"not null;default:main;column:drawer_code"`
	OperationalDate         time.Time  `gorm:"type:date;not null;column:operational_date"`
	Sequence                int        `gorm:"not null;default:1;column:sequence"`
	Status                  string     `gorm:"not null;column:status"`
	CloseDate               time.Time  `gorm:"type:date;not null;column:close_date"`
	OpeningCash             float64    `gorm:"type:numeric(12,2);not null;column:opening_cash"`
	OpeningCashKnown        bool       `gorm:"not null;column:opening_cash_known"`
	ActivityCash            float64    `gorm:"type:numeric(12,2);not null;column:activity_cash"`
	CalculatedCash          float64    `gorm:"type:numeric(12,2);not null;column:calculated_cash"`
	CountedCash             *float64   `gorm:"type:numeric(12,2);column:counted_cash"`
	CashLeft                *float64   `gorm:"type:numeric(12,2);column:cash_left"`
	WithdrawnCash           *float64   `gorm:"type:numeric(12,2);column:withdrawn_cash"`
	WithdrawalDestination   *string    `gorm:"column:withdrawal_destination"`
	DiscrepancyReason       *string    `gorm:"column:discrepancy_reason"`
	CorrectionReason        *string    `gorm:"column:correction_reason"`
	AdjustedAfterWithdrawal bool       `gorm:"not null;column:adjusted_after_withdrawal"`
	IntegrityNote           *string    `gorm:"column:integrity_note"`
	OpenedAt                time.Time  `gorm:"not null;column:opened_at"`
	OpenedBy                uuid.UUID  `gorm:"type:uuid;not null;column:opened_by"`
	ClosedAt                *time.Time `gorm:"column:closed_at"`
	ClosedBy                uuid.UUID  `gorm:"type:uuid;not null;column:closed_by"`
	ReconciledAt            *time.Time `gorm:"column:reconciled_at"`
	ReconciledBy            *uuid.UUID `gorm:"type:uuid;column:reconciled_by"`
	StaleAt                 *time.Time `gorm:"column:stale_at"`
	WithdrawnAt             *time.Time `gorm:"column:withdrawn_at"`
	WithdrawnBy             *uuid.UUID `gorm:"type:uuid;column:withdrawn_by"`
}

func (CashCloseEventModel) TableName() string { return "cash_close_events" }

// CashTransferModel records drawer -> gym_fund delivery separately from the
// close. It never participates in income/expense result formulas.
type CashTransferModel struct {
	ID            uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID         uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version       int        `gorm:"not null;default:1;column:version"`
	CreatedAt     time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt     time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt     *time.Time `gorm:"column:deleted_at"`
	SessionID     uuid.UUID  `gorm:"type:uuid;not null;column:session_id"`
	DrawerID      uuid.UUID  `gorm:"type:uuid;not null;column:drawer_id"`
	Destination   string     `gorm:"not null;column:destination"`
	Amount        float64    `gorm:"type:numeric(12,2);not null;column:amount"`
	TransferredAt time.Time  `gorm:"not null;column:transferred_at"`
	TransferredBy uuid.UUID  `gorm:"type:uuid;not null;column:transferred_by"`
}

func (CashTransferModel) TableName() string { return "cash_transfers" }
