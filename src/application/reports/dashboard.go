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

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
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

	ActiveMembers KPI `json:"active_members"`
	IncomeMonth   KPI `json:"income_month"`
	// RealizedProfitMonth — ganancia realizada de productos del mes en
	// curso vs mismo rango del mes anterior (Standard, sin gate Plus).
	// revenue − COGS sobre ventas NO reembolsadas; el costo es el promedio
	// ponderado all-time del producto. RealizedProfitCoverage lleva la
	// cobertura honesta ("X de Y líneas con costo") por separado.
	RealizedProfitMonth    KPI            `json:"realized_profit_month"`
	RealizedProfitCoverage ProfitCoverage `json:"realized_profit_coverage"`
	// RealizedProfitMarginPct — margen de la utilidad del mes: utilidad /
	// ingreso por productos × 100 (el % de las ventas de productos que fue
	// utilidad). nil cuando no hubo ventas de productos en el rango. Es el
	// número estable de "2 dígitos" que el dueño espera ver, no una tendencia.
	RealizedProfitMarginPct *float64 `json:"realized_profit_margin_pct,omitempty"`
	// ExpensesMonth — egresos del mes corriente vs mismo rango del mes
	// anterior. Suma TRES fuentes: mercancía (stock_movements restock con
	// costo) + gastos generales (BC expenses) + devoluciones (payments
	// concept='refund', en absoluto). Es el número visible en el dashboard
	// para que "egresos" refleje todo lo que sale — un refund es dinero que
	// salió del cajón igual que un gasto (mismo criterio que el corte de
	// caja, ver cash_close.NetTotal).
	ExpensesMonth KPI `json:"expenses_month"`
	// InventoryCostMonth, GeneralExpensesMonth y RefundsMonth — sub-KPIs
	// (no expuestos en el wire hoy, solo el agregado). Existen para que un
	// futuro desglose en UI no requiera tocar el use case.
	InventoryCostMonth   KPI                `json:"-"`
	GeneralExpensesMonth KPI                `json:"-"`
	RefundsMonth         KPI                `json:"-"`
	ExpiringThisWeek     int                `json:"expiring_this_week"`
	RecoverableExpired   int                `json:"recoverable_expired"`
	TodayCash            map[string]float64 `json:"today_cash_by_method"`
	TodayCashTotal       float64            `json:"today_cash_total"`

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

// ProfitCoverage — cobertura honesta de la ganancia realizada: de las
// líneas de venta del período, cuántas tienen costo capturado en su
// producto (las que no, suman a revenue pero no a COGS). El FE lo muestra
// como hint "X de Y con costo".
type ProfitCoverage struct {
	ItemsWithCost int `json:"items_with_cost"`
	ItemsTotal    int `json:"items_total"`
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
	// Gyms (opcional) → "hoy" y fronteras de mes en el día LOCAL del gym
	// (ver localToday). Nil = día UTC (tests viejos).
	Gyms  gymRepo.GymRepository
	cache *dashboardCache
}

// NewDashboard builds the use case with a 60s cache (DA-33.3). Pass ttl=0 to
// disable cache for tests.
func NewDashboard(reader Reader, uow sharedDomain.UnitOfWork, ttl time.Duration) *Dashboard {
	if ttl <= 0 {
		ttl = time.Second // smallest non-zero so Get always returns false
	}
	return &Dashboard{Reader: reader, UoW: uow, cache: newDashboardCache(ttl)}
}

// WithGyms cablea el repo de gyms para anclar el "hoy" del dashboard al
// día calendario del gym en SU zona horaria.
func (uc *Dashboard) WithGyms(g gymRepo.GymRepository) *Dashboard {
	uc.Gyms = g
	return uc
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
	// Día y mes del GYM, no de UTC — desde las 6 PM de CDMX el dashboard
	// mostraba los KPIs del día siguiente.
	today, tzName := localTodayAndTZ(tx, uc.Gyms, in.GymID, now)
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

	// Egresos del mes — agregamos dos fuentes: (1) compras de mercancía
	// vía stock_movements restock con costo, (2) gastos generales del
	// BC expenses. Mismo windowing que ingresos para que los KPIs sean
	// comparables. Cada fuente se mantiene como sub-KPI por si después
	// queremos exponer el desglose en UI.
	inventoryCostMonth, err := uc.Reader.SumInventoryCostBetween(tx, in.GymID, tzName, monthStart, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	inventoryCostPrev, err := uc.Reader.SumInventoryCostBetween(tx, in.GymID, tzName, prevMonthStart, prevMonthEnd)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	generalExpensesMonth, err := uc.Reader.SumExpensesBetween(tx, in.GymID, monthStart, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	generalExpensesPrev, err := uc.Reader.SumExpensesBetween(tx, in.GymID, prevMonthStart, prevMonthEnd)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	// Devoluciones del rango (valor absoluto). IncomeMonth es bruto —
	// SumPaymentsBetween excluye refunds — así que el dinero devuelto sólo
	// aparece en el dashboard si lo contamos como egreso.
	refundsMonth, err := uc.Reader.SumRefundsBetween(tx, in.GymID, monthStart, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	refundsPrev, err := uc.Reader.SumRefundsBetween(tx, in.GymID, prevMonthStart, prevMonthEnd)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	// Ganancia realizada de productos (Standard): revenue − COGS sobre las
	// ventas no reembolsadas del rango. Mismo windowing que ingresos para
	// que el dueño compare "vendí X, gané Y" en el mismo período. La
	// cobertura sale del rango actual (la del previo no se muestra).
	realizedNow, err := uc.Reader.RealizedProductProfitBetween(tx, in.GymID, monthStart, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	realizedPrev, err := uc.Reader.RealizedProductProfitBetween(tx, in.GymID, prevMonthStart, prevMonthEnd)
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
		GeneratedAt:         now,
		ActiveMembers:       newKPI(float64(activeNow), float64(activePrev)),
		IncomeMonth:         newKPI(incomeMonth, incomePrev),
		RealizedProfitMonth: newKPI(realizedNow.Revenue-realizedNow.COGS, realizedPrev.Revenue-realizedPrev.COGS),
		RealizedProfitCoverage: ProfitCoverage{
			ItemsWithCost: realizedNow.ItemsWithCost,
			ItemsTotal:    realizedNow.ItemsTotal,
		},
		RealizedProfitMarginPct: realizedMarginPct(realizedNow),
		InventoryCostMonth:      newKPI(inventoryCostMonth, inventoryCostPrev),
		GeneralExpensesMonth:    newKPI(generalExpensesMonth, generalExpensesPrev),
		RefundsMonth:            newKPI(refundsMonth, refundsPrev),
		ExpensesMonth: newKPI(
			inventoryCostMonth+generalExpensesMonth+refundsMonth,
			inventoryCostPrev+generalExpensesPrev+refundsPrev,
		),
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

// realizedMarginPct devuelve el margen de la utilidad del mes como % del
// ingreso por productos: (Revenue − COGS) / Revenue × 100. El numerador es la
// MISMA utilidad que muestra el KPI, así que el margen es coherente con el
// monto desplegado. nil cuando no hubo ventas de productos (Revenue == 0).
func realizedMarginPct(r RealizedProductProfit) *float64 {
	if r.Revenue <= 0 {
		return nil
	}
	pct := (r.Revenue - r.COGS) / r.Revenue * 100
	return &pct
}

// newKPI builds the standard {current, previous, delta, delta_pct} tile.
// Shared by Dashboard (UC-033) and RangeReport (UC-036). delta_pct stays nil
// when previous == 0 so the FE knows to render the absolute change instead
// of a misleading "+∞%".
func newKPI(current, previous float64) KPI {
	k := KPI{Current: current, Previous: previous, Delta: current - previous}
	if previous != 0 {
		pct := (current - previous) / previous * 100
		k.DeltaPct = &pct
	}
	return k
}
