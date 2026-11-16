package reports

import (
	"math"
	"time"

	"github.com/google/uuid"

	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// CashOpeningReference is what a drawer should hold when it is first opened
// on a day: the cash left at its last count plus the cash that moved after
// that count (late sales, days nobody opened the drawer). Within a day the
// next period is already forced to start with the cash left; this covers the
// night between days, where money can otherwise vanish unnoticed.
type CashOpeningReference struct {
	LastCountDate time.Time
	CashLeft      float64
	LaterCash     float64
	Expected      float64
}

// openingReference returns nil when there is nothing trustworthy to compare
// with: the drawer was never counted, its last session is unfinished, or the
// last count is older than the reminder window.
func (uc *CashClose) openingReference(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID,
	date time.Time, zone string, now time.Time) (*CashOpeningReference, error) {
	date = dayUTC(date)
	previous, err := uc.Events.LatestBefore(tx, gymID, drawerID, date)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if previous == nil || previous.FinishedAt() == nil || previous.CashLeft == nil {
		return nil, nil
	}
	last := dayUTC(previous.OperationalDate)
	if date.Sub(last) > cashPendingWindowDays*24*time.Hour {
		return nil, nil
	}
	later := 0.0
	for day := last; day.Before(date); day = day.AddDate(0, 0, 1) {
		start, _ := tz.DayBounds(zone, day, day)
		if day.Equal(last) {
			start = nextSessionOpenedAt(previous)
		}
		activity, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{
			GymID: gymID, DrawerID: drawerID, OperationalDate: day, OpenedAt: start, AsOf: now,
		})
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		later += activity.Net
	}
	ref := &CashOpeningReference{
		LastCountDate: last,
		CashLeft:      *previous.CashLeft,
		LaterCash:     math.Round(later*100) / 100,
		Expected:      math.Round((*previous.CashLeft+later)*100) / 100,
	}
	if ref.Expected < 0 {
		// More cash left the drawer than was counted in it: the records are
		// incomplete, so there is no honest number to compare the opening to.
		return nil, nil
	}
	return ref, nil
}
