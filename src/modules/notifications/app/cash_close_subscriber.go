package app

import (
	"context"
	"fmt"
	"log"
	"math"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
)

// CashCloseAlertSubscriber adapts EnqueueOwnerAlert to the
// reports.CashCloseSubscriber contract. It's the bridge that fires the
// `cash_close_diff` owner alert when the operator confirms a cierre with a
// non-zero discrepancy.
//
// Lives in notifications/app (not reports/) so the dependency points the
// right way: notifications can know about reports' subscriber interface
// (it's a protocol contract); reports must not know about notifications.
type CashCloseAlertSubscriber struct {
	Enqueue *EnqueueOwnerAlert
}

func NewCashCloseAlertSubscriber(enqueue *EnqueueOwnerAlert) *CashCloseAlertSubscriber {
	return &CashCloseAlertSubscriber{Enqueue: enqueue}
}

// OnCashCloseClosed enqueues an owner_alert_cash_close_diff notification
// when the discrepancy is non-zero. The owner-alert config is consulted
// inside EnqueueOwnerAlert.Execute, so a disabled toggle silently skips.
//
// Errors are swallowed (logged) on purpose — the cierre row has already
// committed by the time we get here, and the operator's HTTP response
// must not be held hostage by a downstream alert hiccup.
func (s *CashCloseAlertSubscriber) OnCashCloseClosed(ctx context.Context, evt reportsApp.CashCloseClosedEvent) {
	if s.Enqueue == nil || evt.Discrepancy == nil {
		return
	}
	if math.Abs(*evt.Discrepancy) < 0.005 {
		// Treat ±half-cent as "matched" — float jitter, not a real diff.
		return
	}
	in := EnqueueOwnerAlertInput{
		GymID: evt.GymID,
		Kind:  OwnerAlertCashCloseDiff,
		Vars: map[string]string{
			"diff_amount": fmt.Sprintf("%.2f", *evt.Discrepancy),
		},
		// One alert per gym per close-day; cierres are one-per-day in the
		// MVP so the date suffix is enough to dedupe.
		IdempotencyKeySuffix: evt.CloseDate.Format("2006-01-02"),
	}
	if _, err := s.Enqueue.Execute(ctx, in); err != nil {
		log.Printf("[notifications/cash_close_alert] enqueue err gym=%s err=%v", evt.GymID, err)
	}
}
