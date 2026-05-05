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

	// AttentionSummary holds the six counts the FE renders as a quick
	// glance into the persecución list. Computed by `len(...)` over the
	// same queries AttentionRequired uses.
	AttentionSummary AttentionSummary `json:"attention_summary"`

	// RecentPayments is the "últimos cobros" widget data — last N
	// non-refund payments, newest first.
	RecentPayments []RecentPaymentRow `json:"recent_payments"`
}

// AttentionSummary holds the six counters surfaced on the dashboard's
// "atención inmediata" tile.
type AttentionSummary struct {
	ExpiringSoon        int `json:"expiring_soon"`
	ExpiredRecoverable  int `json:"expired_recoverable"`
	InactiveInvoluntary int `json:"inactive_involuntary"`
	LowStock            int `json:"low_stock"`
	PendingBalance      int `json:"pending_balance"`
	BirthdaysToday      int `json:"birthdays_today"`
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

	// Attention summary counts (DA-34.1 thresholds — kept in sync with
	// AttentionRequired). Reuses the list queries; for a single gym the
	// dataset is small enough that loading rows just to count them is
	// cheaper than maintaining a parallel set of count queries.
	expiringList, err := uc.Reader.ListExpiringSoon(tx, in.GymID, today, attnExpiringSoonDays)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	expiredList, err := uc.Reader.ListExpiredRecoverable(tx, in.GymID, today, attnRecoverableMaxDays, attnStaleContactDays)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	inactiveList, err := uc.Reader.ListInactiveInvoluntary(tx, in.GymID, today, attnInactiveAbsentDays)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	lowStockList, err := uc.Reader.ListLowStock(tx, in.GymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	pendingList, err := uc.Reader.ListPendingBalances(tx, in.GymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	birthdayList, err := uc.Reader.ListBirthdaysOn(tx, in.GymID, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	recent, err := uc.Reader.ListRecentPayments(tx, in.GymID, recentPaymentsLimit)
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
		AttentionSummary: AttentionSummary{
			ExpiringSoon:        len(expiringList),
			ExpiredRecoverable:  len(expiredList),
			InactiveInvoluntary: len(inactiveList),
			LowStock:            len(lowStockList),
			PendingBalance:      len(pendingList),
			BirthdaysToday:      len(birthdayList),
		},
		RecentPayments: recent,
	}
	uc.cache.Put(in.GymID, out)
	return out, nil
}

// Mirrors of AttentionRequired's thresholds — kept here as separate const
// names so an accidental rename in one place doesn't silently change the
// other. Both stay in lockstep until product asks otherwise.
const (
	attnExpiringSoonDays   = 7
	attnRecoverableMaxDays = 60
	attnStaleContactDays   = 7
	attnInactiveAbsentDays = 21
	recentPaymentsLimit    = 10
)

func newKPI(current, previous float64) KPI {
	k := KPI{Current: current, Previous: previous, Delta: current - previous}
	if previous != 0 {
		pct := (current - previous) / previous * 100
		k.DeltaPct = &pct
	}
	return k
}
