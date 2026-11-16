package reports

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// CashReviewDayInput asks to close out a past drawer-day whose cash can no
// longer be counted on its own.
type CashReviewDayInput struct {
	GymID, ActorUserID uuid.UUID
	ActorRole          string
	Date               time.Time
	DrawerID           *uuid.UUID
	Note               string
}

const cashReviewRefreshReason = "revisión del dueño sin conteo"

// ReviewDay lets the owner resolve a past day left without a certified count:
// a forgotten opening, a payment after the final count, a session left open,
// or a count invalidated by late activity. It closes whatever is pending with
// the current activity and records the owner's review. It never invents a
// count, so the day never reads as "Cuadra"; it only stops asking for
// attention until new physical activity reaches it.
func (uc *CashClose) ReviewDay(ctx context.Context, in CashReviewDayInput) (*CashSessionView, error) {
	now := nowUTC()
	var view *CashSessionView
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		if in.ActorRole != "owner" {
			return sharedDomain.NewBusinessError(cashCloseDomain.ErrReviewOwner, "")
		}
		today, zone, err := cashCommandLocalTodayAndTZ(tx, uc.Gyms, in.GymID, now)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		date := dayUTC(in.Date)
		if in.Date.IsZero() || !date.Before(dayUTC(today)) {
			return sharedDomain.NewValidationError(cashCloseDomain.ErrReviewPastDay)
		}
		drawerID := cashCloseDomain.DefaultDrawerID(in.GymID)
		if in.DrawerID != nil && *in.DrawerID != uuid.Nil {
			drawerID = *in.DrawerID
		}
		if uc.Drawers != nil {
			// A drawer deactivated since then still owns its past days.
			drawer, err := uc.Drawers.GetByID(tx, in.GymID, drawerID)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if drawer == nil {
				return sharedDomain.NewBusinessError(cashCloseDomain.ErrDrawerNotFound, "")
			}
		}
		sessions, err := uc.Events.ListByDate(tx, in.GymID, date)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		var session *cashCloseDomain.CashCloseEvent
		for _, candidate := range sessions {
			if candidate.DrawerID == drawerID && (session == nil || candidate.Sequence > session.Sequence) {
				session = candidate
			}
		}

		if session == nil || session.FinishedAt() != nil {
			// Cash with no session to hold it: a day nobody opened, or a payment
			// after the final count. The new session only claims that activity.
			sequence, opening, openingKnown := 1, 0.0, false
			start, _ := tz.DayBounds(zone, date, date)
			if session != nil {
				sequence = session.Sequence + 1
				start = nextSessionOpenedAt(session)
				if session.CashLeft != nil {
					opening, openingKnown = *session.CashLeft, true
				}
			}
			activity, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{
				GymID: in.GymID, DrawerID: drawerID, OperationalDate: date, OpenedAt: start, AsOf: now,
			})
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if activity.Watermark.IsZero() {
				return sharedDomain.NewBusinessError(cashCloseDomain.ErrNothingToReview, "")
			}
			session, err = cashCloseDomain.Open(in.GymID, drawerID, in.ActorUserID, date, sequence, opening, start)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			// Nobody counted the drawer when this period began.
			session.OpeningCashKnown = openingKnown
			if _, err := uc.Events.Create(tx, session); err != nil {
				if errors.Is(err, billingErrors.ErrCashCloseAlreadyExists) {
					return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseConflict, "la caja cambió; actualiza antes de revisar")
				}
				return sharedDomain.NewUnexpectedError(err)
			}
			if err := uc.recordSessionAudit(ctx, tx, session, in.ActorUserID, audit.ActionCreate, map[string]any{
				"status": session.Status, "opening_cash": session.OpeningCash,
				"opening_cash_known": session.OpeningCashKnown,
			}, now); err != nil {
				return err
			}
		}

		// Each transition is persisted on its own: the repository accepts one
		// version step per write, and the audit keeps every step.
		persist := func(changes map[string]any) error {
			if _, err := uc.Events.Update(tx, session); err != nil {
				if errors.Is(err, billingErrors.ErrCashCloseConflict) {
					return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseConflict, "")
				}
				return sharedDomain.NewUnexpectedError(err)
			}
			return uc.recordSessionAudit(ctx, tx, session, in.ActorUserID, audit.ActionUpdate, changes, now)
		}
		activity, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{
			GymID: in.GymID, DrawerID: drawerID, OperationalDate: date, OpenedAt: session.OpenedAt, AsOf: now,
		})
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		closed := func() map[string]any {
			return map[string]any{"status": session.Status, "activity_cash": session.ActivityCash,
				"expected_cash": session.CalculatedCash}
		}
		late := session.Status != cashCloseDomain.StatusOpen &&
			(math.Abs(activity.Net-session.ActivityCash) > 0.001 ||
				(session.ClosedAt != nil && activity.Watermark.After(*session.ClosedAt)))
		switch session.Status {
		case cashCloseDomain.StatusOpen:
			if err := session.Close(activity.Net, in.ActorUserID, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if err := persist(closed()); err != nil {
				return err
			}
		case cashCloseDomain.StatusStale:
			if err := session.Refresh(activity.Net, cashReviewRefreshReason, in.ActorUserID, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if err := persist(closed()); err != nil {
				return err
			}
		case cashCloseDomain.StatusClosedUnverified, cashCloseDomain.StatusReconciled:
			if !late {
				if session.Status == cashCloseDomain.StatusReconciled || session.ReviewedWithoutCount() {
					return sharedDomain.NewBusinessError(cashCloseDomain.ErrNothingToReview, "")
				}
				break
			}
			// The earlier count no longer matches the activity; the audit
			// keeps it, the session moves on without it.
			if err := session.MarkStale(cashReviewRefreshReason, now); err != nil {
				return sharedDomain.NewBusinessError(err, "")
			}
			if err := persist(map[string]any{"status": session.Status, "reason": cashReviewRefreshReason}); err != nil {
				return err
			}
			if err := session.Refresh(activity.Net, cashReviewRefreshReason, in.ActorUserID, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if err := persist(closed()); err != nil {
				return err
			}
		default:
			return sharedDomain.NewBusinessError(cashCloseDomain.ErrInvalidSessionState, "")
		}
		if err := session.ReviewWithoutCount(in.Note, in.ActorUserID, now); err != nil {
			if errors.Is(err, cashCloseDomain.ErrInvalidSessionState) {
				return sharedDomain.NewBusinessError(err, "")
			}
			return sharedDomain.NewValidationError(err)
		}
		if err := persist(map[string]any{
			"status": session.Status, "reviewed_without_count": true, "review_note": session.ReviewNote,
		}); err != nil {
			return err
		}
		view = cashSessionView(session, session.Status, false, session.CalculatedCash)
		return nil
	})
	return view, err
}

// cashDayNeedsReview mirrors what ReviewDay can resolve, so the screen only
// offers the action when it will succeed.
func cashDayNeedsReview(out *CashCloseReportOutput) bool {
	if out.Session == nil || out.Session.FinishedAt != nil {
		return out.RequiresNewSession
	}
	switch out.Session.Status {
	case cashCloseDomain.StatusOpen, cashCloseDomain.StatusStale:
		return true
	case cashCloseDomain.StatusClosedUnverified:
		return !out.Session.ReviewedWithoutCount
	}
	return false
}
