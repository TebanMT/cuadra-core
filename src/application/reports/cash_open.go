package reports

import (
	"context"
	"errors"
	"time"

	cash "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	domain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
	"github.com/google/uuid"
)

type CashOpenInput struct {
	GymID, ActorUserID uuid.UUID
	Date               time.Time
	DrawerID           *uuid.UUID
	OpeningCash        float64
	// Expected slot makes retrying an opening safe even if someone already closed it.
	Sequence int
}

func (uc *CashClose) Open(ctx context.Context, in CashOpenInput) (*CashSessionView, error) {
	now := nowUTC()
	var out *CashSessionView
	err := uc.UoW.Command(ctx, func(tx domain.Transaction) error {
		today, zone, err := cashCommandLocalTodayAndTZ(tx, uc.Gyms, in.GymID, now)
		if err != nil {
			return domain.NewUnexpectedError(err)
		}
		if in.Date.IsZero() {
			in.Date = today
		}
		in.Date = dayUTC(in.Date)
		if !sameDay(in.Date, today) {
			return domain.NewValidationError(errors.New("la caja se abre para el día de hoy"))
		}
		drawerID := cash.DefaultDrawerID(in.GymID)
		if in.DrawerID != nil {
			drawerID = *in.DrawerID
		}
		if uc.Drawers != nil {
			drawer, err := uc.Drawers.GetByID(tx, in.GymID, drawerID)
			if err != nil {
				return domain.NewUnexpectedError(err)
			}
			if drawer == nil || !drawer.Active {
				return domain.NewBusinessError(cash.ErrDrawerInactive, "")
			}
		}
		periods, err := uc.Events.ListByDate(tx, in.GymID, in.Date)
		if err != nil {
			return domain.NewUnexpectedError(err)
		}
		var previous *cash.CashCloseEvent
		for _, p := range periods {
			if p.DrawerID == drawerID {
				if p.Sequence == in.Sequence {
					if p.OpeningCash != in.OpeningCash {
						return domain.NewBusinessError(billingErrors.ErrCashCloseConflict, "el efectivo inicial ya fue registrado; actualiza la caja")
					}
					out = cashSessionView(p, p.Status, p.Status == cash.StatusStale, p.CalculatedCash)
					return nil
				}
				if previous == nil || p.Sequence > previous.Sequence {
					previous = p
				}
			}
		}
		sequence := 1
		start, _ := tz.DayBounds(zone, in.Date, in.Date)
		if previous != nil {
			if previous.FinishedAt() == nil {
				return domain.NewBusinessError(billingErrors.ErrCashCloseConflict, "hay un corte pendiente; complétalo antes de abrir otro periodo")
			}
			sequence = previous.Sequence + 1
			start = nextSessionOpenedAt(previous)
			// The previous counted stock is already known. Changing it would hide a
			// shortage between periods; cash added/removed is a separate movement.
			if previous.CashLeft == nil || *previous.CashLeft != in.OpeningCash {
				return domain.NewValidationError(errors.New("el siguiente periodo comienza con el efectivo que quedó en caja"))
			}
		}
		if in.Sequence != sequence {
			return domain.NewBusinessError(billingErrors.ErrCashCloseConflict, "la caja cambió; actualiza antes de abrir")
		}
		session, err := cash.Open(in.GymID, drawerID, in.ActorUserID, in.Date, sequence, in.OpeningCash, start)
		if err != nil {
			return domain.NewValidationError(err)
		}
		if _, err = uc.Events.Create(tx, session); err != nil {
			return domain.NewUnexpectedError(err)
		}
		if err = uc.recordSessionAudit(ctx, tx, session, in.ActorUserID, audit.ActionCreate, map[string]any{"opening_cash": session.OpeningCash, "status": session.Status}, now); err != nil {
			return err
		}
		out = cashSessionView(session, session.Status, false, session.CalculatedCash)
		return nil
	})
	return out, err
}
