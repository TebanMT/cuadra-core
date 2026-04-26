// UC-033 — Dashboard del dueño.
//
// Composes a handful of cross-context aggregates into a single read model
// that the owner consults from the web. DA-33.3 caches the response per gym
// for 60s; subsequent calls within that window skip every query.
package reports

import (
	"context"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// DashboardInput identifies the gym whose KPIs to aggregate. The "today"
// reference is server-clock UTC at call time — the use case derives all
// windowing from it. Operators in different timezones still see consistent
// numbers because every record in the DB is also UTC (CLAUDE.md storage
// convention).
type DashboardInput struct {
	GymID uuid.UUID
}

// DashboardOutput is the read model the controller marshals.
type DashboardOutput struct {
	GeneratedAt time.Time `json:"generated_at"`

	ActiveMembers      KPI                `json:"active_members"`
	IncomeMonth        KPI                `json:"income_month"`
	ExpiringThisWeek   int                `json:"expiring_this_week"`
	RecoverableExpired int                `json:"recoverable_expired"`
	TodayCash          map[string]float64 `json:"today_cash_by_method"`
	TodayCashTotal     float64            `json:"today_cash_total"`

	IncomeLast30Days []DailyIncome `json:"income_last_30_days"`
}

// KPI is the typical "value + delta" tile.
type KPI struct {
	Current  float64  `json:"current"`
	Previous float64  `json:"previous"`
	Delta    float64  `json:"delta"`               // current - previous
	DeltaPct *float64 `json:"delta_pct,omitempty"` // nil when previous == 0
}

// Dashboard is the UC-033 use case.
type Dashboard struct {
	Reader Reader
	UoW    sharedDomain.UnitOfWork
	cache  *dashboardCache
}

// NewDashboard builds the use case with a 60s cache (DA-33.3). Pass ttl=0 to
// disable cache for tests.
func NewDashboard(reader Reader, uow sharedDomain.UnitOfWork, ttl time.Duration) *Dashboard {
	if ttl <= 0 {
		ttl = time.Second // smallest non-zero so Get always returns false
	}
	return &Dashboard{Reader: reader, UoW: uow, cache: newDashboardCache(ttl)}
}

// InvalidateCache drops the cached entry for a gym. Wired to the persecución
// mutations so the operator sees the change immediately after marking a socio.
func (uc *Dashboard) InvalidateCache(gymID uuid.UUID) {
	uc.cache.Invalidate(gymID)
}

// Execute runs the aggregation. Read-only — UoW.Query().
func (uc *Dashboard) Execute(ctx context.Context, in DashboardInput) (*DashboardOutput, error) {
	if cached, ok := uc.cache.Get(in.GymID); ok {
		return cached, nil
	}
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	prevMonthStart := monthStart.AddDate(0, -1, 0)
	prevMonthEnd := monthStart.AddDate(0, 0, -1)

	activeNow, err := uc.Reader.CountActiveMembers(tx, in.GymID, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	// Trend: active count one month ago. Approximate with same-day in
	// previous month (DA-33.1 keeps it simple).
	activePrev, err := uc.Reader.CountActiveMembers(tx, in.GymID, today.AddDate(0, -1, 0))
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	incomeMonth, err := uc.Reader.SumPaymentsBetween(tx, in.GymID, monthStart, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	// Trend: same date-range length in previous month.
	incomePrev, err := uc.Reader.SumPaymentsBetween(tx, in.GymID, prevMonthStart, prevMonthEnd)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	expiringWeek, err := uc.Reader.CountExpiringBetween(tx, in.GymID, today, today.AddDate(0, 0, 7))
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	recoverable, err := uc.Reader.CountExpiredRecoverable(tx, in.GymID, today, 60)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	todayCash, err := uc.Reader.TodayCashByMethod(tx, in.GymID, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	var totalToday float64
	for _, v := range todayCash {
		totalToday += v
	}
	series, err := uc.Reader.IncomeDailySeries(tx, in.GymID, today.AddDate(0, 0, -29), today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	out := &DashboardOutput{
		GeneratedAt:        now,
		ActiveMembers:      newKPI(float64(activeNow), float64(activePrev)),
		IncomeMonth:        newKPI(incomeMonth, incomePrev),
		ExpiringThisWeek:   expiringWeek,
		RecoverableExpired: recoverable,
		TodayCash:          todayCash,
		TodayCashTotal:     totalToday,
		IncomeLast30Days:   series,
	}
	uc.cache.Put(in.GymID, out)
	return out, nil
}

func newKPI(current, previous float64) KPI {
	k := KPI{Current: current, Previous: previous, Delta: current - previous}
	if previous != 0 {
		pct := (current - previous) / previous * 100
		k.DeltaPct = &pct
	}
	return k
}
