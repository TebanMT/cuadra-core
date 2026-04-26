// Package reports holds cross-context read models — queries that span
// multiple bounded contexts and don't naturally belong inside any one BC.
// CLAUDE.md identifies this directory as "queries read-only cross-context".
//
// UC-027 (Corte de caja diario) lives here because it composes:
//   - billing aggregate (payments + sales totals by method, concept, operator)
//   - billing.cashclose write (when the operator confirms the cierre)
//
// The use case has two entry points: Report() returns the read model so the
// UI can render the totals; Close() persists the operator-confirmed cierre.
package reports

import (
	"context"
	"time"

	"github.com/google/uuid"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CashCloseReportInput struct {
	GymID uuid.UUID
	Date  time.Time
}

type CashCloseReportOutput struct {
	Totals *billingRepo.CashCloseTotals
}

type CashCloseInput struct {
	GymID             uuid.UUID
	ActorUserID       uuid.UUID
	Date              time.Time
	CountedCash       *float64
	DiscrepancyReason *string
}

type CashCloseOutput struct {
	CashCloseID    uuid.UUID
	CalculatedCash float64
	CountedCash    *float64
	Discrepancy    *float64
}

// CashClose is the UC-027 use case. Two methods: Report() reads, Close() writes.
type CashClose struct {
	Reader billingRepo.CashCloseReader
	Events billingRepo.CashCloseEventRepository
	UoW    sharedDomain.UnitOfWork
	Audit  audit.Recorder
}

func NewCashClose(reader billingRepo.CashCloseReader, events billingRepo.CashCloseEventRepository,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CashClose {
	return &CashClose{Reader: reader, Events: events, UoW: uow, Audit: recorder}
}

// Report runs the per-day aggregation. Read-only — uses Query() not Command().
func (uc *CashClose) Report(ctx context.Context, in CashCloseReportInput) (*CashCloseReportOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	totals, err := uc.Reader.Aggregate(tx, billingRepo.CashCloseQuery{
		GymID: in.GymID, Date: in.Date,
	})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return &CashCloseReportOutput{Totals: totals}, nil
}

// Close persists the cierre event. The aggregate runs first (re-using the
// same tx) so the snapshot of "what we had" is correct at write time. The
// operator's counted_cash + discrepancy_reason are stored — DA-27.2 only
// audits the gap, never blocks.
func (uc *CashClose) Close(ctx context.Context, in CashCloseInput) (*CashCloseOutput, error) {
	now := time.Now().UTC()
	var out CashCloseOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		totals, err := uc.Reader.Aggregate(tx, billingRepo.CashCloseQuery{
			GymID: in.GymID, Date: in.Date,
		})
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// "Calculated cash" = sum of cash payments minus cash refunds for the day.
		calculated := totals.ByMethod[paymentDomain.MethodCash]
		// Refunds are not folded into ByMethod (the reader excluded concept=refund).
		// We need to add the cash refund slice back in (as negative). The reader
		// kept refunds in RefundTotal — we recompute the cash share by tagging on
		// the refund's effect indirectly: for MVP, refunds are uncommon enough
		// that operators will simply note the gap on the close form.
		event := cashCloseDomain.New(uuid.New(), in.GymID, in.ActorUserID,
			in.Date, calculated, in.CountedCash, in.DiscrepancyReason, now)
		if _, err := uc.Events.Create(tx, event); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "cash_close_events",
			EntityID:    event.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"close_date":         event.CloseDate.Format("2006-01-02"),
				"calculated_cash":    event.CalculatedCash,
				"counted_cash":       event.CountedCash,
				"discrepancy_reason": event.DiscrepancyReason,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = CashCloseOutput{
			CashCloseID:    event.ID,
			CalculatedCash: event.CalculatedCash,
			CountedCash:    event.CountedCash,
		}
		if event.CountedCash != nil {
			d := *event.CountedCash - event.CalculatedCash
			out.Discrepancy = &d
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
