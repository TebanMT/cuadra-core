package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CashSessionAdjustmentMarker bridges an edit of an already-recorded physical
// drawer movement with the cash-session certification that covered it. The
// implementation runs in the caller's transaction so the movement edit and
// the certification invalidation cannot diverge.
//
// Creating a new late movement does not use this port: its CreatedAt timestamp
// lets the cash-session reader derive stale/uncovered activity directly.
type CashSessionAdjustmentMarker interface {
	MarkPhysicalCashAdjustment(context.Context, sharedDomain.Transaction, CashSessionAdjustmentInput) error
}

type CashSessionAdjustmentInput struct {
	GymID              uuid.UUID
	ActorUserID        uuid.UUID
	OperationalDate    time.Time
	OriginalRecordedAt time.Time
	DrawerID           uuid.UUID
	Reason             string
}
