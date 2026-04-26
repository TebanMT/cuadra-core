// Package cashclose holds the CashCloseEvent entity (UC-027). The aggregate
// is intentionally tiny: it captures the operator's end-of-day cierre so the
// audit log + reports can show "what was the cash count vs what Cuadra
// calculated, and what did the operator say about the gap".
package cashclose

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// CashCloseEvent is one row in `cash_close_events`. `discrepancy` is computed
// by the database (GENERATED column) from counted_cash - calculated_cash.
type CashCloseEvent struct {
	ID                uuid.UUID
	GymID             uuid.UUID
	Version           int
	CloseDate         time.Time
	CalculatedCash    float64
	CountedCash       *float64
	DiscrepancyReason *string
	ClosedBy          uuid.UUID
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
}

// New constructs a CashCloseEvent. `countedCash` is optional — when nil the
// operator skipped the "verify cash" step.
func New(id, gymID, closedBy uuid.UUID, closeDate time.Time,
	calculatedCash float64, countedCash *float64, reason *string, now time.Time) *CashCloseEvent {
	if reason != nil {
		r := strings.TrimSpace(*reason)
		if r == "" {
			reason = nil
		} else {
			reason = &r
		}
	}
	return &CashCloseEvent{
		ID:                id,
		GymID:             gymID,
		Version:           1,
		CloseDate:         truncateDate(closeDate),
		CalculatedCash:    calculatedCash,
		CountedCash:       countedCash,
		DiscrepancyReason: reason,
		ClosedBy:          closedBy,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}

func truncateDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
