package reports

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// Why a drawer-day still holds cash without a valid count.
const (
	CashPendingNoOpening  = "no_opening"  // cash moved and nobody opened the drawer
	CashPendingOpen       = "open"        // the session was never closed
	CashPendingNotCounted = "not_counted" // closed without counting
	CashPendingChanged    = "changed"     // activity changed after the count
	CashPendingAfterClose = "after_close" // cash arrived after the final count
)

// cashPendingWindowDays bounds the reminder to the recent habit; older gaps
// stay in the period report instead of nagging every morning.
const cashPendingWindowDays = 7

type CashPendingDaysInput struct {
	GymID uuid.UUID
	// IncludeToday also reports today's drawers (end-of-shift reminder).
	IncludeToday bool
}

type CashPendingDay struct {
	Date          time.Time
	IsToday       bool
	DrawerID      uuid.UUID
	DrawerName    string
	IsMainDrawer  bool
	Reason        string
	ExpectedCash  *float64
	UncoveredCash float64
	// CountNow: the drawer has not been touched since that day (no later
	// session, no later cash), so counting it now is still that day's count.
	CountNow bool
	Session  *CashSessionView
}

// PendingDays lists recent drawer-days whose cash still lacks a valid count,
// newest first. It mirrors CashCloseReportOutput.PendingReview for past days
// and only starts once the gym has really opened a drawer, so a gym that
// never adopted the corte habit is not flooded with historical gaps.
func (uc *CashClose) PendingDays(ctx context.Context, in CashPendingDaysInput) ([]CashPendingDay, error) {
	out := []CashPendingDay{}
	now := nowUTC()
	err := sharedDomain.ReadSnapshot(ctx, uc.UoW, func(tx sharedDomain.Transaction) error {
		today, zone := localTodayAndTZ(tx, uc.Gyms, in.GymID, now)
		today = dayUTC(today)
		recent, err := uc.Events.ListByGym(tx, in.GymID, 100)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		var trackedSince *time.Time
		for _, session := range recent {
			if !session.OpeningCashKnown {
				continue
			}
			day := dayUTC(session.OperationalDate)
			if trackedSince == nil || day.Before(*trackedSince) {
				trackedSince = &day
			}
		}
		if trackedSince == nil {
			return nil
		}
		mainID := cashCloseDomain.DefaultDrawerID(in.GymID)
		names := map[uuid.UUID]string{mainID: cashCloseDomain.DefaultDrawerName}
		if uc.Drawers != nil {
			catalog, err := uc.Drawers.ListByGym(tx, in.GymID, true)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			for _, drawer := range catalog {
				names[drawer.ID] = drawer.Name
			}
		}

		// Walking from today backwards tells whether a drawer was touched
		// after the day being evaluated.
		touchedLater := map[uuid.UUID]bool{}
		for offset := 0; offset <= cashPendingWindowDays; offset++ {
			date := today.AddDate(0, 0, -offset)
			if date.Before(*trackedSince) {
				break
			}
			sessions, err := uc.Events.ListByDate(tx, in.GymID, date)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			physical, err := uc.Reader.CashDrawers(tx, in.GymID, date)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			seen := map[uuid.UUID]bool{}
			for _, session := range sessions {
				seen[session.DrawerID] = true
			}
			for _, activity := range physical {
				seen[activity.DrawerID] = true
			}
			drawers := make([]uuid.UUID, 0, len(seen))
			for id := range seen {
				drawers = append(drawers, id)
			}
			sort.Slice(drawers, func(i, j int) bool {
				if (drawers[i] == mainID) != (drawers[j] == mainID) {
					return drawers[i] == mainID
				}
				return names[drawers[i]] < names[drawers[j]]
			})
			for _, drawerID := range drawers {
				if offset > 0 || in.IncludeToday {
					item, pending, err := uc.pendingDrawerDay(tx, in.GymID, drawerID, date, zone, sessions, now)
					if err != nil {
						return err
					}
					if pending {
						item.DrawerName = names[drawerID]
						if item.DrawerName == "" {
							item.DrawerName = cashCloseDomain.DrawerCode(in.GymID, drawerID)
						}
						item.IsMainDrawer = drawerID == mainID
						item.IsToday = offset == 0
						item.CountNow = offset > 0 && !touchedLater[drawerID] && item.Session != nil &&
							item.Session.OpeningCashKnown &&
							(item.Reason == CashPendingOpen || item.Reason == CashPendingNotCounted)
						out = append(out, item)
					}
				}
				touchedLater[drawerID] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pendingDrawerDay applies the same rules as Report + cashDayNeedsReview to
// one drawer-day, without the ledger and expense reads the screen needs.
func (uc *CashClose) pendingDrawerDay(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID, date time.Time,
	zone string, daySessions []*cashCloseDomain.CashCloseEvent, now time.Time) (CashPendingDay, bool, error) {
	item := CashPendingDay{Date: date, DrawerID: drawerID}
	var latest *cashCloseDomain.CashCloseEvent
	for _, session := range daySessions {
		if session.DrawerID == drawerID && (latest == nil || session.Sequence > latest.Sequence) {
			latest = session
		}
	}
	activityFrom := func(start time.Time) (*billingRepo.CashSessionActivity, error) {
		activity, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{
			GymID: gymID, DrawerID: drawerID, OperationalDate: date, OpenedAt: start, AsOf: now,
		})
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		return activity, nil
	}
	if latest == nil || latest.FinishedAt() != nil {
		start, _ := tz.DayBounds(zone, date, date)
		item.Reason = CashPendingNoOpening
		if latest != nil {
			start = nextSessionOpenedAt(latest)
			item.Reason = CashPendingAfterClose
		}
		activity, err := activityFrom(start)
		if err != nil {
			return item, false, err
		}
		if activity.Watermark.IsZero() {
			return item, false, nil
		}
		item.UncoveredCash = activity.Net
		return item, true, nil
	}
	activity, err := activityFrom(latest.OpenedAt)
	if err != nil {
		return item, false, err
	}
	outdated := latest.Status == cashCloseDomain.StatusStale ||
		(latest.Status != cashCloseDomain.StatusOpen &&
			(math.Abs(activity.Net-latest.ActivityCash) > 0.001 ||
				(latest.ClosedAt != nil && activity.Watermark.After(*latest.ClosedAt))))
	switch {
	case latest.Status == cashCloseDomain.StatusOpen:
		item.Reason = CashPendingOpen
	case outdated:
		item.Reason = CashPendingChanged
	case latest.Status == cashCloseDomain.StatusClosedUnverified && !latest.ReviewedWithoutCount():
		item.Reason = CashPendingNotCounted
	default:
		return item, false, nil
	}
	expected := latest.OpeningCash + activity.Net
	if latest.OpeningCashKnown {
		item.ExpectedCash = &expected
	}
	item.Session = cashSessionView(latest, latest.EffectiveStatus(outdated), outdated, expected)
	return item, true, nil
}
