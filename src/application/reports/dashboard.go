// UC-033 — Dashboard del dueño.
//
// Composes a handful of cross-context aggregates into a single read model
// that the owner consults from the web. Money is always read fresh inside one
// database snapshot: a short-lived server cache made a just-recorded payment
// disagree with the reports page and with desktop sync.
package reports

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
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
	LocalDate     string     `json:"local_date"`
	Timezone      string     `json:"timezone"`
	PreviousFrom  string     `json:"previous_from"`
	PreviousTo    string     `json:"previous_to"`
	GeneratedAt   time.Time  `json:"generated_at"`
	DataWatermark *time.Time `json:"data_watermark"`
	SyncPending   *bool      `json:"sync_pending"`

	ActiveMembers           KPI                `json:"active_members"`
	IncomeMonth             KPI                `json:"income_month"`
	MembershipIncomeMonth   KPI                `json:"membership_income_month"`
	ProductIncomeMonth      KPI                `json:"product_income_month"`
	OtherIncomeMonth        KPI                `json:"other_income_month"`
	UnclassifiedIncomeMonth KPI                `json:"unclassified_income_month"`
	OperatingExpensesMonth  KPI                `json:"operating_expenses_month"`
	InventoryPurchasesMonth KPI                `json:"inventory_purchases_month"`
	PeriodResultMonth       KPI                `json:"period_result_month"`
	Integrity               FinancialIntegrity `json:"integrity"`
	// RealizedProfitMonth — ganancia realizada de productos del mes en
	// curso vs mismo rango del mes anterior. Es análisis Plus: nil en
	// Trial/Standard para que el contrato no filtre el resultado premium.
	// revenue − COGS en base de caja: cobros/abonos reconocen costo
	// proporcional y refunds lo revierten. El costo es el snapshot congelado
	// en cada línea al vender. RealizedProfitCoverage lleva la
	// cobertura honesta ("X de Y líneas con costo") por separado.
	RealizedProfitMonth    *KPI            `json:"realized_profit_month,omitempty"`
	RealizedProfitCoverage *ProfitCoverage `json:"realized_profit_coverage,omitempty"`
	// RealizedProfitMarginPct — margen de la utilidad del mes: utilidad /
	// ingreso por productos × 100 (el % de las ventas de productos que fue
	// utilidad). nil cuando no hubo ventas de productos en el rango. Es el
	// número estable de "2 dígitos" que el dueño espera ver, no una tendencia.
	RealizedProfitMarginPct *float64 `json:"realized_profit_margin_pct,omitempty"`
	// ExpensesMonth is the backwards-compatible aggregate now defined as all
	// period outflows: paid inventory purchases + paid operating expenses +
	// physical revenue refunds. It is not COGS and is independent of cash
	// drawer reconciliation.
	ExpensesMonth KPI `json:"expenses_month"`
	// Legacy aliases kept for old internal callers during the transition.
	InventoryCostMonth   KPI                `json:"-"`
	GeneralExpensesMonth KPI                `json:"-"`
	RefundsMonth         KPI                `json:"refunds_month"`
	ExpiringThisWeek     int                `json:"expiring_this_week"`
	RecoverableExpired   int                `json:"recoverable_expired"`
	TodayCash            map[string]float64 `json:"today_cash_by_method"`
	TodayCashTotal       float64            `json:"today_cash_total"`
	// CheckinsToday — entradas de HOY en el día local del gym. Pieza del
	// home operacional del operador (plan Reports-improve transversal §2).
	CheckinsToday int `json:"checkins_today"`

	IncomeLast30Days []DailyIncome `json:"income_last_30_days"`

	// Summary shared by contextual links and older clients.
	AttentionSummary AttentionSummary `json:"attention_summary"`

	// RecentPayments is the "últimos cobros" widget data — last N
	// non-refund payments, newest first.
	RecentPayments []RecentPaymentRow `json:"recent_payments"`
}

// AttentionSummary retains its wire shape for older desktops.
// BirthdaysToday is retired and always zero.
type AttentionSummary struct {
	ExpiringSoon        int `json:"expiring_soon"`
	ExpiredRecoverable  int `json:"expired_recoverable"`
	InactiveInvoluntary int `json:"inactive_involuntary"`
	LowStock            int `json:"low_stock"`
	PendingBalance      int `json:"pending_balance"`
	BirthdaysToday      int `json:"birthdays_today"`
}

// ProfitCoverage — cobertura honesta de la ganancia realizada: de los
// productos con actividad en el período, cuántos tienen costo completo en
// todas sus líneas (los demás suman a revenue, pero su margen queda oculto).
// El FE lo muestra como hint "X de Y con costo".
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
	Gyms gymRepo.GymRepository
}

// NewDashboard keeps ttl in its signature so existing composition roots do
// not break. The value is intentionally ignored: financial reads must reflect
// a committed mutation immediately.
func NewDashboard(reader Reader, uow sharedDomain.UnitOfWork, ttl time.Duration) *Dashboard {
	_ = ttl
	return &Dashboard{Reader: reader, UoW: uow}
}

// WithGyms cablea el repo de gyms para anclar el "hoy" del dashboard al
// día calendario del gym en SU zona horaria.
func (uc *Dashboard) WithGyms(g gymRepo.GymRepository) *Dashboard {
	uc.Gyms = g
	return uc
}

// InvalidateCache remains as a no-op compatibility hook for controllers that
// used to invalidate the removed dashboard cache.
func (uc *Dashboard) InvalidateCache(gymID uuid.UUID) {
	_ = gymID
}

// Execute runs every component inside one repeatable read snapshot.
func (uc *Dashboard) Execute(ctx context.Context, in DashboardInput) (*DashboardOutput, error) {
	var out *DashboardOutput
	err := sharedDomain.ReadSnapshot(ctx, uc.UoW, func(tx sharedDomain.Transaction) error {
		var executeErr error
		out, executeErr = uc.executeInSnapshot(tx, in)
		return executeErr
	})
	if err != nil {
		var custom sharedDomain.CustomError
		if errors.As(err, &custom) {
			return nil, err
		}
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return out, nil
}

func (uc *Dashboard) executeInSnapshot(tx sharedDomain.Transaction, in DashboardInput) (*DashboardOutput, error) {
	now := time.Now().UTC()
	var readMetadata FinancialReadMetadata
	if metadataReader, ok := uc.Reader.(FinancialMetadataReader); ok {
		var err error
		readMetadata, err = metadataReader.FinancialReadMetadata(tx, in.GymID)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
	}
	// Día y mes del GYM, no de UTC — desde las 6 PM de CDMX el dashboard
	// mostraba los KPIs del día siguiente.
	today, tzName := localTodayAndTZ(tx, uc.Gyms, in.GymID, now)
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	prevMonthStart := monthStart.AddDate(0, -1, 0)
	// Compare MTD against the same number of calendar days in the previous
	// month. Comparing 1–12 August against all of July inflated the baseline
	// and made every current-month KPI look artificially weak.
	prevMonthEnd := previousMonthMTDEnd(today)

	activeNow, err := uc.Reader.CountActiveMembers(tx, in.GymID, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	// Trend: active count one month ago. Approximate with same-day in
	// previous month (DA-33.1 keeps it simple).
	activePrev, err := uc.Reader.CountActiveMembers(tx, in.GymID, prevMonthEnd)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	var canonicalNow, canonicalPrev CanonicalFinancialSnapshot
	if canonicalReader, ok := uc.Reader.(CanonicalFinancialReader); ok {
		canonicalNow, err = canonicalReader.CanonicalFinancialBetween(tx, in.GymID, tzName, monthStart, today)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		canonicalPrev, err = canonicalReader.CanonicalFinancialBetween(tx, in.GymID, tzName, prevMonthStart, prevMonthEnd)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
	} else {
		// Compatibility path for old adapters and small test doubles. Production
		// PostgreSQL and SQLite readers always use the canonical query above.
		incomeMonth, readErr := uc.Reader.SumPaymentsBetween(tx, in.GymID, monthStart, today)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		incomePrev, readErr := uc.Reader.SumPaymentsBetween(tx, in.GymID, prevMonthStart, prevMonthEnd)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		otherMonth, readErr := uc.Reader.SumOtherIncomeBetween(tx, in.GymID, monthStart, today)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		otherPrev, readErr := uc.Reader.SumOtherIncomeBetween(tx, in.GymID, prevMonthStart, prevMonthEnd)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		productMonth, readErr := uc.Reader.SumProductSalesBetween(tx, in.GymID, monthStart, today)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		productPrev, readErr := uc.Reader.SumProductSalesBetween(tx, in.GymID, prevMonthStart, prevMonthEnd)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		inventoryMonth, readErr := uc.Reader.SumInventoryCostBetween(tx, in.GymID, tzName, monthStart, today)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		inventoryPrev, readErr := uc.Reader.SumInventoryCostBetween(tx, in.GymID, tzName, prevMonthStart, prevMonthEnd)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		expensesMonth, readErr := uc.Reader.SumExpensesBetween(tx, in.GymID, monthStart, today)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		expensesPrev, readErr := uc.Reader.SumExpensesBetween(tx, in.GymID, prevMonthStart, prevMonthEnd)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		refundsMonth, readErr := uc.Reader.SumRefundsBetween(tx, in.GymID, monthStart, today)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		refundsPrev, readErr := uc.Reader.SumRefundsBetween(tx, in.GymID, prevMonthStart, prevMonthEnd)
		if readErr != nil {
			return nil, sharedDomain.NewUnexpectedError(readErr)
		}
		canonicalNow = canonicalFallback(incomeMonth, otherMonth, productMonth.Amount, expensesMonth, inventoryMonth, refundsMonth)
		canonicalPrev = canonicalFallback(incomePrev, otherPrev, productPrev.Amount, expensesPrev, inventoryPrev, refundsPrev)
	}

	incomeMonth := canonicalIncome(canonicalNow)
	incomePrev := canonicalIncome(canonicalPrev)
	inventoryCostMonth := canonicalNow.InventoryPurchases
	inventoryCostPrev := canonicalPrev.InventoryPurchases
	generalExpensesMonth := canonicalNow.OperatingExpenses
	generalExpensesPrev := canonicalPrev.OperatingExpenses
	refundsMonth := canonicalNow.Refunds
	refundsPrev := canonicalPrev.Refunds
	outflowsMonth := canonicalOutflows(canonicalNow)
	outflowsPrev := canonicalOutflows(canonicalPrev)
	periodResultMonth := canonicalPeriodResult(canonicalNow)
	periodResultPrev := canonicalPeriodResult(canonicalPrev)

	// Ganancia y margen por producto pertenecen a Plus. El gate se aplica
	// antes de consultar: Standard no recibe ceros ambiguos ni datos de los
	// que pueda derivar el análisis premium.
	var realizedKPI *KPI
	var realizedCoverage *ProfitCoverage
	var realizedProfitMarginPct *float64
	if uc.canAccessPlus(tx, in.GymID) {
		realizedNow, err := uc.Reader.RealizedProductProfitBetween(tx, in.GymID, monthStart, today)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		realizedPrev, err := uc.Reader.RealizedProductProfitBetween(tx, in.GymID, prevMonthStart, prevMonthEnd)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		kpi := newKPI(realizedNow.Revenue-realizedNow.COGS, realizedPrev.Revenue-realizedPrev.COGS)
		coverage := ProfitCoverage{ItemsWithCost: realizedNow.ItemsWithCost, ItemsTotal: realizedNow.ItemsTotal}
		realizedKPI = &kpi
		realizedCoverage = &coverage
		realizedProfitMarginPct = realizedMarginPct(realizedNow)
	}

	expiringWeek, err := uc.Reader.CountExpiringBetween(tx, in.GymID, today, today.AddDate(0, 0, 7))
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	recoverable, err := uc.Reader.CountExpiredRecoverable(tx, in.GymID, today, 60)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	checkinsToday, err := uc.Reader.CountCheckinsBetween(tx, in.GymID, tzName, today, today)
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

	recent, err := uc.Reader.ListRecentPayments(tx, in.GymID, recentPaymentsLimit)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	out := &DashboardOutput{
		LocalDate:               today.Format("2006-01-02"),
		Timezone:                tzName,
		PreviousFrom:            prevMonthStart.Format("2006-01-02"),
		PreviousTo:              prevMonthEnd.Format("2006-01-02"),
		GeneratedAt:             now,
		DataWatermark:           readMetadata.DataWatermark,
		SyncPending:             readMetadata.SyncPending,
		ActiveMembers:           newKPI(float64(activeNow), float64(activePrev)),
		IncomeMonth:             newKPI(incomeMonth, incomePrev),
		MembershipIncomeMonth:   newKPI(canonicalNow.MembershipIncome, canonicalPrev.MembershipIncome),
		ProductIncomeMonth:      newKPI(canonicalNow.ProductIncome, canonicalPrev.ProductIncome),
		OtherIncomeMonth:        newKPI(canonicalNow.OtherIncome, canonicalPrev.OtherIncome),
		UnclassifiedIncomeMonth: newKPI(canonicalNow.UnclassifiedIncome, canonicalPrev.UnclassifiedIncome),
		OperatingExpensesMonth:  newKPI(generalExpensesMonth, generalExpensesPrev),
		InventoryPurchasesMonth: newKPI(inventoryCostMonth, inventoryCostPrev),
		RefundsMonth:            newKPI(refundsMonth, refundsPrev),
		ExpensesMonth:           newKPI(outflowsMonth, outflowsPrev),
		PeriodResultMonth:       newKPI(periodResultMonth, periodResultPrev),
		Integrity:               integrityFromSnapshot(canonicalNow),
		RealizedProfitMonth:     realizedKPI,
		RealizedProfitCoverage:  realizedCoverage,
		RealizedProfitMarginPct: realizedProfitMarginPct,
		InventoryCostMonth:      newKPI(inventoryCostMonth, inventoryCostPrev),
		GeneralExpensesMonth:    newKPI(generalExpensesMonth, generalExpensesPrev),
		ExpiringThisWeek:        expiringWeek,
		RecoverableExpired:      recoverable,
		CheckinsToday:           checkinsToday,
		TodayCash:               todayCash,
		TodayCashTotal:          totalToday,
		IncomeLast30Days:        series,
		AttentionSummary: AttentionSummary{
			ExpiringSoon:        len(expiringList),
			ExpiredRecoverable:  len(expiredList),
			InactiveInvoluntary: len(inactiveList),
			LowStock:            len(lowStockList),
			PendingBalance:      len(pendingList),
			BirthdaysToday:      0, // Compatibility for older clients.
		},
		RecentPayments: recent,
	}
	return out, nil
}

func (uc *Dashboard) canAccessPlus(tx sharedDomain.Transaction, gymID uuid.UUID) bool {
	if uc.Gyms == nil {
		return false
	}
	g, err := uc.Gyms.GetByID(tx, gymID)
	return err == nil && g != nil && gymDomain.CanAccessPlusFeatures(g.SubscriptionPlan)
}

func previousMonthMTDEnd(today time.Time) time.Time {
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	prevMonthStart := monthStart.AddDate(0, -1, 0)
	end := prevMonthStart.AddDate(0, 0, today.Day()-1)
	lastPrevMonthDay := monthStart.AddDate(0, 0, -1)
	if end.After(lastPrevMonthDay) {
		return lastPrevMonthDay
	}
	return end
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
	pct = roundReportValue(pct)
	return &pct
}

// newKPI builds the standard {current, previous, delta, delta_pct} tile.
// Shared by Dashboard (UC-033) and RangeReport (UC-036). delta_pct stays nil
// when previous == 0 so the FE knows to render the absolute change instead
// of a misleading "+∞%".
func newKPI(current, previous float64) KPI {
	current, previous = roundReportValue(current), roundReportValue(previous)
	k := KPI{Current: current, Previous: previous, Delta: roundReportValue(current - previous)}
	if previous != 0 {
		pct := roundReportValue(k.Delta / previous * 100)
		k.DeltaPct = &pct
	}
	return k
}

func roundReportValue(value float64) float64 {
	return math.Round(value*100) / 100
}
