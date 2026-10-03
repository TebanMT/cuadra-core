package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CashSessionCorrectionMarker is the atomic bridge from an administrative
// sale correction to the physical drawer model. The implementation receives
// the correction Command's transaction: changing the recorded payment and
// invalidating its cash reconciliation must commit or roll back together.
type CashSessionCorrectionMarker interface {
	MarkRecordCorrection(context.Context, sharedDomain.Transaction, CashSessionAdjustmentInput) error
}

type CashSessionAdjustmentInput struct {
	GymID              uuid.UUID
	ActorUserID        uuid.UUID
	OperationalDate    time.Time
	OriginalRecordedAt time.Time
	Reason             string
	DrawerID           *uuid.UUID
}
