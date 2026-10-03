// UC-036 — Reportes por período. Composes the range page (KPIs + breakdowns
// + daily series + tables) out of the Reader queries. The Reader is the same
// one the dashboard uses; main.go shares the singleton.
//
// Each total surfaces as a KPI {current, previous, delta, delta_pct} so the
// FE can render trend chips like the dashboard does. The "previous window"
// is calculated by previousWindow() — same length as the selected period,
// terminating one day before from.
package reports

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ReportPeriod mirrors the FE enum.
const (
	PeriodToday     = "today"
	PeriodWeek      = "week"
	PeriodMonth     = "month"
	PeriodLastMonth = "last_month"
	Period3Months   = "3_months"
	PeriodYear      = "year"
	PeriodCustom    = "custom"
)

// topMembersLimit caps the leaderboards at 5 — same as the FE renders.
const topMembersLimit = 5
const topProductsLimit = 5
const rangeDetailRowsLimit = 200

type RangeReportInput struct {
	GymID  uuid.UUID
	Period string
	// From/To are used for custom ranges and frozen export ranges. Inclusive, day-grain.
	From *time.Time
	To   *time.Time
	// IncludeAllDetails is reserved for trusted server-side exports. The HTTP
	// range response stays bounded and declares truncation explicitly; a PDF or
	// XLSX must never silently drop row 201 onward.
	IncludeAllDetails bool `json:"-"`
	// Frozen exports retain the original period comparison and the visible dates.
	fixedWindow bool
}

// RangeReportOutput matches the FE ReportsRangeData shape.
type RangeReportOutput struct {
	PreviousFrom       string             `json:"previous_from"`
	PreviousTo         string             `json:"previous_to"`
	CalculatedAt       time.Time          `json:"calculated_at"`
	DataWatermark      *time.Time         `json:"data_watermark"`
	SyncPending        *bool              `json:"sync_pending"`
	Period             string             `json:"period"`
	From               string             `json:"from"`
	To                 string             `json:"to"`
	Totals             RangeTotals        `json:"totals"`
	CashReconciliation CashReconciliation `json:"cash_reconciliation"`
	// ProductSales — KPI "Ventas de productos": $ con trend + unidades del
	// período actual (la sub-línea "· N uds" es informativa, sin delta).
	ProductSales       ProductSalesKPI    `json:"product_sales"`
	IncomeByDay        []DailyIncome      `json:"income_by_day"`
	ExpensesByDay      []DailyAmount      `json:"expenses_by_day"`
	CheckinsByDay      []DailyCount       `json:"checkins_by_day"`
	IncomeByMethod     map[string]float64 `json:"income_by_method"`
	ExpensesByCategory map[string]float64 `json:"expenses_by_category"`
	// IncomeByMembershipType / MembersByType — breakdowns Standard por tipo
	// de membresía: cobros de membresía del período y snapshot de socios
	// activos (la suma de MembersByType cuadra con el KPI de activos).
	IncomeByMembershipType map[string]float64 `json:"income_by_membership_type"`
	MembersByType          map[string]int     `json:"members_by_membership_type"`
	TopMembers             []TopMemberRow     `json:"top_members"`
	TopProducts            []TopProductRow    `json:"top_products"`
	// InventoryCosts — compras explícitas pagadas del período.
	InventoryCosts []InventoryCostRow `json:"inventory_costs"`
	// Expenses — gastos generales (BC expenses) del período.
	Expenses []ExpenseRow `json:"expenses"`
	// CriticalStock — snapshot actual de stock (no varía con período).
	CriticalStock        CriticalStockCounts   `json:"critical_stock"`
	Integrity            FinancialIntegrity    `json:"integrity"`
	ProductProfitability *ProductProfitability `json:"product_profitability,omitempty"`
	DetailMetadata       RangeDetailMetadata   `json:"detail_metadata"`
}

type RangeDetailMetadata struct {
	InventoryCosts RangeListMetadata `json:"inventory_costs"`
	Expenses       RangeListMetadata `json:"expenses"`
}

type RangeListMetadata struct {
	Returned  int  `json:"returned"`
	Limit     *int `json:"limit,omitempty"`
	Truncated bool `json:"truncated"`
}

type ProductProfitability struct {
	Status        string                    `json:"status"`
	ItemsWithCost int                       `json:"items_with_cost"`
	ItemsTotal    int                       `json:"items_total"`
	Rows          []ProductProfitabilityRow `json:"rows"`
}

type FinancialIntegrity struct {
	Status                           string   `json:"status"`
	Warnings                         []string `json:"warnings"`
	UnclassifiedIncomeCount          int      `json:"unclassified_income_count"`
	UnclassifiedCashOutCount         int      `json:"unclassified_cash_out_count"`
	InvalidCashInClassificationCount int      `json:"invalid_cash_in_classification_count"`
	LegacyCashSourceUnverifiedCount  int      `json:"legacy_cash_source_unverified_count"`
	MissingPurchaseAmountCount       int      `json:"missing_purchase_amount_count"`
	// LegacyPurchaseCount is the one-release wire alias retained for clients
	// that still read the old field name. It now counts only purchases whose
	// amount truly cannot be reconstructed, not every pre-aggregate receipt.
	LegacyPurchaseCount int `json:"legacy_purchase_count"`
	LegacyRefundCount   int `json:"legacy_refund_count"`
}

// CashReconciliation deliberately separates period flow from one physical
// snapshot. A count includes opening cash; period activity does not. Summing
// counts or subtracting them from activity would manufacture discrepancies.
type CashReconciliation struct {
	Status             string     `json:"status"`
	Complete           bool       `json:"complete"`
	RequiresAttention  bool       `json:"requires_attention"`
	PeriodActivity     float64    `json:"period_activity"`
	PeriodWithdrawn    float64    `json:"period_withdrawn"`
	LatestSessionID    *uuid.UUID `json:"latest_session_id,omitempty"`
	LatestExpected     *float64   `json:"latest_expected,omitempty"`
	LatestCounted      *float64   `json:"latest_counted,omitempty"`
	LatestDifference   *float64   `json:"latest_difference"`
	LatestCountedAt    *time.Time `json:"latest_counted_at,omitempty"`
	LatestStatus       string     `json:"latest_status,omitempty"`
	LatestNeedsRecount bool       `json:"latest_needs_recount"`
	// These counters partition tracked, non-deleted sessions in the period by their
	// effective status. A late movement turns a previously reconciled session
	// into stale for reporting, even before its persisted status catches up.
	HistoricalActivityDays          int `json:"historical_activity_days"`
	HistoricalSessions              int `json:"historical_sessions"`
	ActiveDays                      int `json:"active_days"`
	MissingActiveDays               int `json:"missing_active_days"`
	UncoveredActivityDays           int `json:"uncovered_activity_days"`
	OpenSessions                    int `json:"open_sessions"`
	ClosedUnverifiedSessions        int `json:"closed_unverified_sessions"`
	ReconciledSessions              int `json:"reconciled_sessions"`
	StaleSessions                   int `json:"stale_sessions"`
	WithdrawnSessions               int `json:"withdrawn_sessions"`
	UnknownOpeningSessions          int `json:"unknown_opening_sessions"`
	AdjustedAfterWithdrawalSessions int `json:"adjusted_after_withdrawal_sessions"`
	// Historical expenses with an ambiguous source are not silently applied to
	// the drawer. The financial result remains valid, but physical cash cannot
	// be certified until an owner confirms Caja or Fondo.
	LegacyCashSourceUnverifiedCount int `json:"legacy_cash_source_unverified_count"`
	ActiveSessions                  int `json:"active_sessions"`
	TotalSessions                   int `json:"total_sessions"`
	CountedCloses                   int `json:"counted_closes"`
	TotalCloses                     int `json:"total_closes"`
	// One-release aliases now mean the latest snapshot, never an aggregate.
	Counted    float64  `json:"counted"`
	Difference *float64 `json:"difference,omitempty"`
}

func cashReconciliationCoverage(summary CashCountSummary, totalSessions, reconciledSessions int) (string, bool, bool) {
	if summary.StaleSessions > 0 {
		return "stale", false, true
	}
	resolved := reconciledSessions + summary.WithdrawnSessions
	complete := summary.MissingActiveDays == 0 &&
		summary.UncoveredActivityDays == 0 &&
		summary.OpenSessions == 0 &&
		summary.ClosedUnverifiedSessions == 0 &&
		summary.UnknownOpeningSessions == 0 &&
		summary.AdjustedAfterWithdrawalSessions == 0 &&
		summary.LegacyCashSourceUnverifiedCount == 0 &&
		resolved == totalSessions &&
		summary.CountedCloses == summary.TotalCloses
	if complete {
		// Historical coverage is unknown, but cannot become a retroactive task.
		if summary.HistoricalActivityDays > 0 || summary.HistoricalSessions > 0 {
			return "incomplete", false, false
		}
		return "complete", true, false
	}
	return "incomplete", false, true
}

// RangeTotals is the FE-facing totals block. Each numeric value carries a
// previous-window comparison so the StatCards can render deltas.
// PeriodResult follows the deliberately simple cash-basis model used by Tinta:
// all income − product purchases − paid expenses − refunds.
// CashFromCloses is a legacy wire name. Its value is the complete physical
// cash activity of the period, including days without a close and open or
// post-withdrawal activity. Physical counts, coverage and discrepancies remain
// separate reconciliation data.
type RangeTotals struct {
	Income             KPI `json:"income"`
	MembershipIncome   KPI `json:"membership_income"`
	ProductIncome      KPI `json:"product_income"`
	OtherIncome        KPI `json:"other_income"`
	UnclassifiedIncome KPI `json:"unclassified_income"`
	OperatingExpenses  KPI `json:"operating_expenses"`
	InventoryPurchases KPI `json:"inventory_purchases"`
	Outflows           KPI `json:"outflows"`
	PeriodResult       KPI `json:"period_result"`
	InventoryCost      KPI `json:"inventory_cost"`
	ExpensesGeneral    KPI `json:"expenses_general"`
	Refunds            KPI `json:"refunds"`
	NewMembers         KPI `json:"new_members"`
	Checkins           KPI `json:"checkins"`
	// Deprecated: use PeriodResult. Kept for one compatibility window.
	Net KPI `json:"net"`
	// Deprecated: use PeriodResult. Kept for one compatibility window.
	NetResult KPI `json:"net_result"`
	// Deprecated: use PeriodResult. Kept for one compatibility window.
	OperatingResult KPI `json:"operating_result"`
	// Deprecated: use CashFromCloses. Kept for one compatibility window.
	CashFlow       KPI `json:"cash_flow"`
	CashFromCloses KPI `json:"cash_from_closes"`
}

// ProductSalesKPI — bloque del KPI "Ventas de productos": monto con
// comparación vs ventana previa + unidades vendidas del período actual.
type ProductSalesKPI struct {
	Amount KPI `json:"amount"`
	Units  int `json:"units"`
}

// CriticalStockCounts — productos sin stock o por debajo del mínimo. Es un
// snapshot del estado actual del catálogo, no del período seleccionado.
type CriticalStockCounts struct {
	OutCount int `json:"out_count"`
	LowCount int `json:"low_count"`
}

// DailyCount is `{date, count}[]` — used for the checkins-by-day chart.
type DailyCount struct {
	Date  time.Time `json:"date"`
	Count int       `json:"count"`
}

// DailyAmount is `{date, total}[]` — used for the expenses-by-day series.
// Same shape as DailyIncome but kept separate so the field semantics stay
// explicit at the Reader interface.
type DailyAmount struct {
	Date  time.Time `json:"date"`
	Total float64   `json:"total"`
}

// TopMemberRow is one entry in the "top socios pagadores" leaderboard.
type TopMemberRow struct {
	MemberID      uuid.UUID `json:"member_id"`
	FullName      string    `json:"full_name"`
	TotalPaid     float64   `json:"total_paid"`
	PaymentsCount int       `json:"payments_count"`
}

// TopProductRow is one entry of the "top productos vendidos" leaderboard.
type TopProductRow struct {
	ProductID   uuid.UUID `json:"product_id"`
	ProductName string    `json:"product_name"`
	Quantity    int       `json:"quantity"`
	Revenue     float64   `json:"revenue"`
}

// RangeReport is the UC-036 use case.
type RangeReport struct {
	Reader Reader
	UoW    sharedDomain.UnitOfWork
	// Gyms (opcional) → ancla el "hoy" del período al día LOCAL del gym
	// (ver localToday). Nil = día UTC (tests viejos).
	Gyms gymRepo.GymRepository
	// Now (opcional) → reloj inyectable para tests. Nil = time.Now().UTC().
	Now func() time.Time
}

// now resuelve el reloj del use case (inyectable en tests).
func (uc *RangeReport) now() time.Time {
	if uc.Now != nil {
		return uc.Now().UTC()
	}
	return time.Now().UTC()
}

func NewRangeReport(reader Reader, uow sharedDomain.UnitOfWork) *RangeReport {
	return &RangeReport{Reader: reader, UoW: uow}
}

// WithGyms cablea el repo de gyms para anclar el período al día calendario
// del gym en SU zona horaria.
func (uc *RangeReport) WithGyms(g gymRepo.GymRepository) *RangeReport {
	uc.Gyms = g
	return uc
}

func (uc *RangeReport) Execute(ctx context.Context, in RangeReportInput) (*RangeReportOutput, error) {
	if err := validateReportWindow(in.Period, in.From, in.To); err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	var out *RangeReportOutput
	err := sharedDomain.ReadSnapshot(ctx, uc.UoW, func(tx sharedDomain.Transaction) error {
		var executeErr error
		out, executeErr = uc.executeInSnapshot(ctx, tx, in)
		return executeErr
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (uc *RangeReport) executeInSnapshot(ctx context.Context, tx sharedDomain.Transaction, in RangeReportInput) (*RangeReportOutput, error) {
	// El "hoy" del período es el día del GYM, no el de UTC — desde las 6 PM
	// de CDMX "Hoy" resolvía al día siguiente y salía en ceros aunque hubiera
	// cobros. Mismo anclaje que el dashboard (localToday).
	today, tzName := localTodayAndTZ(tx, uc.Gyms, in.GymID, uc.now())
	windowPeriod := in.Period
	if in.fixedWindow {
		windowPeriod = PeriodCustom
	}
	from, to, err := ResolveWindow(windowPeriod, in.From, in.To, today)
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	prevFrom, prevTo := previousWindow(in.Period, from, to)

	out := &RangeReportOutput{
		PreviousFrom:           prevFrom.Format("2006-01-02"),
		PreviousTo:             prevTo.Format("2006-01-02"),
		CalculatedAt:           uc.now(),
		Period:                 in.Period,
		From:                   from.Format("2006-01-02"),
		To:                     to.Format("2006-01-02"),
		IncomeByDay:            []DailyIncome{},
		ExpensesByDay:          []DailyAmount{},
		CheckinsByDay:          []DailyCount{},
		IncomeByMethod:         map[string]float64{},
		ExpensesByCategory:     map[string]float64{},
		IncomeByMembershipType: map[string]float64{},
		MembersByType:          map[string]int{},
		TopMembers:             []TopMemberRow{},
		TopProducts:            []TopProductRow{},
		InventoryCosts:         []InventoryCostRow{},
		Expenses:               []ExpenseRow{},
		Integrity:              FinancialIntegrity{Status: "complete", Warnings: []string{}},
	}
	if metadataReader, ok := uc.Reader.(FinancialMetadataReader); ok {
		metadata, err := metadataReader.FinancialReadMetadata(tx, in.GymID)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		out.DataWatermark = metadata.DataWatermark
		out.SyncPending = metadata.SyncPending
	}

	// Ningún error financiero se convierte en cero: el endpoint falla de
	// forma visible para no presentar una cifra inventada como dato válido.
	incomeNow, err := uc.Reader.SumPaymentsBetween(tx, in.GymID, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	incomePrev, err := uc.Reader.SumPaymentsBetween(tx, in.GymID, prevFrom, prevTo)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out.Totals.Income = newKPI(incomeNow, incomePrev)

	otherIncomeNow, err := uc.Reader.SumOtherIncomeBetween(tx, in.GymID, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	otherIncomePrev, err := uc.Reader.SumOtherIncomeBetween(tx, in.GymID, prevFrom, prevTo)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out.Totals.OtherIncome = newKPI(otherIncomeNow, otherIncomePrev)

	cashClosedNow, err := uc.Reader.SumCashClosedBetween(tx, in.GymID, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	cashClosedPrev, err := uc.Reader.SumCashClosedBetween(tx, in.GymID, prevFrom, prevTo)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	cashClosedNow, cashClosedPrev = roundReportValue(cashClosedNow), roundReportValue(cashClosedPrev)
	out.Totals.CashFromCloses = newKPI(cashClosedNow, cashClosedPrev)
	countedCash, err := uc.Reader.SumCashCountedBetween(tx, in.GymID, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	totalSessions := countedCash.TotalSessions
	if totalSessions == 0 && countedCash.TotalCloses > 0 {
		// Compatibility for Reader adapters from the previous release. Current
		// SQL readers always provide TotalSessions and the status partition.
		totalSessions = countedCash.TotalCloses
	}
	reconciledSessions := countedCash.ReconciledSessions
	if countedCash.OpenSessions+countedCash.ClosedUnverifiedSessions+countedCash.ReconciledSessions+
		countedCash.StaleSessions+countedCash.WithdrawnSessions == 0 && totalSessions > 0 {
		reconciledSessions = countedCash.CountedCloses
	}
	status, complete, requiresAttention := cashReconciliationCoverage(countedCash, totalSessions, reconciledSessions)
	latestExpected := roundReportPointer(countedCash.LatestExpected)
	latestCounted := roundReportPointer(countedCash.LatestCounted)
	latestDifference := roundReportPointer(countedCash.LatestDifference)
	if countedCash.LatestNeedsRecount || countedCash.LatestStatus == "stale" {
		latestDifference = nil
	}
	out.CashReconciliation = CashReconciliation{
		Status:                          status,
		Complete:                        complete,
		PeriodActivity:                  cashClosedNow,
		PeriodWithdrawn:                 roundReportValue(countedCash.Withdrawn),
		LatestSessionID:                 countedCash.LatestSessionID,
		LatestExpected:                  latestExpected,
		LatestCounted:                   latestCounted,
		LatestDifference:                latestDifference,
		LatestCountedAt:                 countedCash.LatestCountedAt,
		LatestStatus:                    countedCash.LatestStatus,
		LatestNeedsRecount:              countedCash.LatestNeedsRecount,
		RequiresAttention:               requiresAttention,
		HistoricalActivityDays:          countedCash.HistoricalActivityDays,
		HistoricalSessions:              countedCash.HistoricalSessions,
		ActiveDays:                      countedCash.ActiveDays,
		MissingActiveDays:               countedCash.MissingActiveDays,
		UncoveredActivityDays:           countedCash.UncoveredActivityDays,
		OpenSessions:                    countedCash.OpenSessions,
		ClosedUnverifiedSessions:        countedCash.ClosedUnverifiedSessions,
		ReconciledSessions:              reconciledSessions,
		StaleSessions:                   countedCash.StaleSessions,
		WithdrawnSessions:               countedCash.WithdrawnSessions,
		UnknownOpeningSessions:          countedCash.UnknownOpeningSessions,
		AdjustedAfterWithdrawalSessions: countedCash.AdjustedAfterWithdrawalSessions,
		LegacyCashSourceUnverifiedCount: countedCash.LegacyCashSourceUnverifiedCount,
		ActiveSessions:                  totalSessions,
		TotalSessions:                   totalSessions,
		CountedCloses:                   countedCash.CountedCloses,
		TotalCloses:                     countedCash.TotalCloses,
	}
	if latestCounted != nil {
		out.CashReconciliation.Counted = *latestCounted
		out.CashReconciliation.Difference = latestDifference
	} else if countedCash.LatestSessionID == nil && countedCash.TotalCloses > 0 && countedCash.CountedCloses == countedCash.TotalCloses {
		// Compatibility for one-release Reader adapters that only populate the
		// old aggregate shape. Production readers always return latest fields.
		out.CashReconciliation.Counted = roundReportValue(countedCash.Counted)
	}

	inventoryNow, err := uc.Reader.SumInventoryCostBetween(tx, in.GymID, tzName, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	inventoryPrev, err := uc.Reader.SumInventoryCostBetween(tx, in.GymID, tzName, prevFrom, prevTo)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out.Totals.InventoryCost = newKPI(inventoryNow, inventoryPrev)

	expensesNow, err := uc.Reader.SumExpensesBetween(tx, in.GymID, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	expensesPrev, err := uc.Reader.SumExpensesBetween(tx, in.GymID, prevFrom, prevTo)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out.Totals.ExpensesGeneral = newKPI(expensesNow, expensesPrev)

	refundsNow, err := uc.Reader.SumRefundsBetween(tx, in.GymID, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	refundsPrev, err := uc.Reader.SumRefundsBetween(tx, in.GymID, prevFrom, prevTo)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out.Totals.Refunds = newKPI(refundsNow, refundsPrev)

	newMembersNow, err := uc.Reader.CountNewMembersBetween(tx, in.GymID, tzName, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	newMembersPrev, err := uc.Reader.CountNewMembersBetween(tx, in.GymID, tzName, prevFrom, prevTo)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out.Totals.NewMembers = newKPI(float64(newMembersNow), float64(newMembersPrev))

	checkinsNow, err := uc.Reader.CountCheckinsBetween(tx, in.GymID, tzName, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	checkinsPrev, err := uc.Reader.CountCheckinsBetween(tx, in.GymID, tzName, prevFrom, prevTo)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out.Totals.Checkins = newKPI(float64(checkinsNow), float64(checkinsPrev))

	productNow, err := uc.Reader.SumProductSalesBetween(tx, in.GymID, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	productPrev, err := uc.Reader.SumProductSalesBetween(tx, in.GymID, prevFrom, prevTo)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out.ProductSales = ProductSalesKPI{
		Amount: newKPI(productNow.Amount, productPrev.Amount),
		Units:  productNow.Units,
	}

	// Canonical Standard model. Production readers implement the interface;
	// the fallback keeps old test adapters wire-compatible for one release.
	canonicalNow := canonicalFallback(incomeNow, otherIncomeNow, productNow.Amount, expensesNow, inventoryNow, refundsNow)
	canonicalPrev := canonicalFallback(incomePrev, otherIncomePrev, productPrev.Amount, expensesPrev, inventoryPrev, refundsPrev)
	if canonicalReader, ok := uc.Reader.(CanonicalFinancialReader); ok {
		canonicalNow, err = canonicalReader.CanonicalFinancialBetween(tx, in.GymID, tzName, from, to)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		canonicalPrev, err = canonicalReader.CanonicalFinancialBetween(tx, in.GymID, tzName, prevFrom, prevTo)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
	}
	incomeNow = canonicalIncome(canonicalNow)
	incomePrev = canonicalIncome(canonicalPrev)
	inventoryNow, inventoryPrev = canonicalNow.InventoryPurchases, canonicalPrev.InventoryPurchases
	expensesNow, expensesPrev = canonicalNow.OperatingExpenses, canonicalPrev.OperatingExpenses
	refundsNow, refundsPrev = canonicalNow.Refunds, canonicalPrev.Refunds
	out.Totals.Income = newKPI(incomeNow, incomePrev)
	out.Totals.MembershipIncome = newKPI(canonicalNow.MembershipIncome, canonicalPrev.MembershipIncome)
	out.Totals.ProductIncome = newKPI(canonicalNow.ProductIncome, canonicalPrev.ProductIncome)
	out.Totals.OtherIncome = newKPI(canonicalNow.OtherIncome, canonicalPrev.OtherIncome)
	out.Totals.UnclassifiedIncome = newKPI(canonicalNow.UnclassifiedIncome, canonicalPrev.UnclassifiedIncome)
	out.Totals.OperatingExpenses = newKPI(expensesNow, expensesPrev)
	out.Totals.InventoryPurchases = newKPI(inventoryNow, inventoryPrev)
	out.Totals.InventoryCost = out.Totals.InventoryPurchases
	out.Totals.ExpensesGeneral = out.Totals.OperatingExpenses
	out.Totals.Refunds = newKPI(refundsNow, refundsPrev)
	out.ProductSales.Amount = out.Totals.ProductIncome
	out.Integrity = integrityFromSnapshot(canonicalNow)

	// Resultado del período = todos los ingresos − compras − gastos − devoluciones.
	// Se descuenta la compra completa al pagarla: es el modelo sencillo que el
	// operador puede reconciliar sin nociones de costo contable de mercancía.
	netNow := canonicalPeriodResult(canonicalNow)
	netPrev := canonicalPeriodResult(canonicalPrev)
	out.Totals.NetResult = newKPI(netNow, netPrev)
	outflowsNow := canonicalOutflows(canonicalNow)
	outflowsPrev := canonicalOutflows(canonicalPrev)
	out.Totals.Outflows = newKPI(outflowsNow, outflowsPrev)
	out.Totals.PeriodResult = out.Totals.NetResult
	// Aliases para versiones anteriores de dashboard/desktop.
	out.Totals.Net = out.Totals.NetResult
	out.Totals.OperatingResult = out.Totals.NetResult
	out.Totals.CashFlow = out.Totals.CashFromCloses

	// Daily series (charts).
	series, err := uc.Reader.IncomeDailySeries(tx, in.GymID, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if series != nil {
		out.IncomeByDay = series
	}
	expenseSeries, err := uc.Reader.ExpensesDailySeries(tx, in.GymID, tzName, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if expenseSeries != nil {
		out.ExpensesByDay = expenseSeries
	}
	checkinSeries, err := uc.Reader.CheckinsDailySeries(tx, in.GymID, tzName, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if checkinSeries != nil {
		out.CheckinsByDay = checkinSeries
	}

	// Breakdowns + leaderboards.
	byMethod, err := uc.Reader.IncomeByMethodBetween(tx, in.GymID, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if byMethod != nil {
		out.IncomeByMethod = byMethod
	}
	byCategory, err := uc.Reader.ExpensesByCategoryBetween(tx, in.GymID, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if byCategory != nil {
		out.ExpensesByCategory = byCategory
	}
	byMembership, err := uc.Reader.IncomeByMembershipTypeBetween(tx, in.GymID, from, to)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if byMembership != nil {
		out.IncomeByMembershipType = byMembership
	}
	membersByType, err := uc.Reader.ActiveMembersByType(tx, in.GymID, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if membersByType != nil {
		out.MembersByType = membersByType
	}
	topMembers, err := uc.Reader.TopMembersBetween(tx, in.GymID, from, to, topMembersLimit)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if topMembers != nil {
		out.TopMembers = topMembers
	}
	topProducts, err := uc.Reader.TopProductsBetween(tx, in.GymID, from, to, topProductsLimit)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if topProducts != nil {
		out.TopProducts = topProducts
	}
	if profitabilityReader, ok := uc.Reader.(ProductProfitabilityReader); ok && uc.canAccessPlus(tx, in.GymID) {
		rows, err := profitabilityReader.ProductProfitabilityBetween(tx, in.GymID, from, to)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		profitability := &ProductProfitability{Status: "complete", Rows: rows, ItemsTotal: len(rows)}
		for _, row := range rows {
			if row.CostComplete {
				profitability.ItemsWithCost++
			}
		}
		if profitability.ItemsWithCost != profitability.ItemsTotal {
			profitability.Status = "incomplete"
		}
		out.ProductProfitability = profitability
	}

	// Tablas detalladas del período. La respuesta interactiva pide una fila
	// extra para poder declarar truncation con certeza; el export solicita el
	// conjunto completo mediante el valor negativo reservado del Reader.
	detailQueryLimit := rangeDetailRowsLimit + 1
	if in.IncludeAllDetails {
		detailQueryLimit = -1
	}
	inventoryRows, err := uc.Reader.ListInventoryCostsBetween(tx, in.GymID, tzName, from, to, detailQueryLimit)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if !in.IncludeAllDetails {
		limit := rangeDetailRowsLimit
		out.DetailMetadata.InventoryCosts.Limit = &limit
		if len(inventoryRows) > rangeDetailRowsLimit {
			out.DetailMetadata.InventoryCosts.Truncated = true
			inventoryRows = inventoryRows[:rangeDetailRowsLimit]
		}
	}
	if inventoryRows != nil {
		out.InventoryCosts = inventoryRows
	}
	out.DetailMetadata.InventoryCosts.Returned = len(out.InventoryCosts)
	expenseRows, err := uc.Reader.ListExpensesBetween(tx, in.GymID, from, to, detailQueryLimit)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if !in.IncludeAllDetails {
		limit := rangeDetailRowsLimit
		out.DetailMetadata.Expenses.Limit = &limit
		if len(expenseRows) > rangeDetailRowsLimit {
			out.DetailMetadata.Expenses.Truncated = true
			expenseRows = expenseRows[:rangeDetailRowsLimit]
		}
	}
	if expenseRows != nil {
		out.Expenses = expenseRows
	}
	out.DetailMetadata.Expenses.Returned = len(out.Expenses)

	// Snapshot de stock crítico (no varía con período).
	critical, err := uc.Reader.CountCriticalStock(tx, in.GymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out.CriticalStock = critical

	return out, nil
}

func (uc *RangeReport) canAccessPlus(tx sharedDomain.Transaction, gymID uuid.UUID) bool {
	if uc.Gyms == nil {
		return false
	}
	g, err := uc.Gyms.GetByID(tx, gymID)
	return err == nil && g != nil && gymDomain.CanAccessPlusFeatures(g.SubscriptionPlan)
}

func canonicalFallback(income, other, product, expenses, purchases, refunds float64) CanonicalFinancialSnapshot {
	membership := income - other - product
	if membership < 0 {
		membership = 0
	}
	return CanonicalFinancialSnapshot{
		MembershipIncome: membership, ProductIncome: product, OtherIncome: other,
		OperatingExpenses: expenses, InventoryPurchases: purchases, Refunds: refunds,
	}
}

func roundReportPointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	rounded := roundReportValue(*value)
	return &rounded
}

func canonicalIncome(v CanonicalFinancialSnapshot) float64 {
	return roundReportValue(v.MembershipIncome + v.ProductIncome + v.OtherIncome + v.UnclassifiedIncome)
}

func canonicalOutflows(v CanonicalFinancialSnapshot) float64 {
	return roundReportValue(v.OperatingExpenses + v.InventoryPurchases + v.Refunds)
}

func canonicalPeriodResult(v CanonicalFinancialSnapshot) float64 {
	return roundReportValue(canonicalIncome(v) - canonicalOutflows(v))
}

func integrityFromSnapshot(v CanonicalFinancialSnapshot) FinancialIntegrity {
	out := FinancialIntegrity{
		Status: "complete", Warnings: []string{},
		UnclassifiedIncomeCount:          v.UnclassifiedIncomeCount,
		UnclassifiedCashOutCount:         v.UnclassifiedCashOutCount,
		InvalidCashInClassificationCount: v.InvalidCashInClassificationCount,
		LegacyCashSourceUnverifiedCount:  v.LegacyCashSourceUnverifiedCount,
		MissingPurchaseAmountCount:       v.LegacyPurchaseCount,
		LegacyPurchaseCount:              v.LegacyPurchaseCount,
		LegacyRefundCount:                v.LegacyRefundCount,
	}
	if v.UnclassifiedIncomeCount > 0 {
		out.Warnings = append(out.Warnings, "unclassified_income")
	}
	if v.UnclassifiedCashOutCount > 0 {
		out.Warnings = append(out.Warnings, "unclassified_cash_out")
	}
	if v.InvalidCashInClassificationCount > 0 {
		out.Warnings = append(out.Warnings, "invalid_cash_in_classification")
	}
	if v.LegacyCashSourceUnverifiedCount > 0 {
		out.Warnings = append(out.Warnings, "legacy_cash_source_unverified")
	}
	if v.LegacyPurchaseCount > 0 {
		out.Warnings = append(out.Warnings, "legacy_inventory_purchase")
	}
	if v.LegacyRefundCount > 0 {
		out.Warnings = append(out.Warnings, "legacy_refund")
	}
	// A legacy cash-source ambiguity affects only physical drawer
	// reconciliation: the expense still belongs to the period result. Keep the
	// financial status complete when it is the only warning, so clients do not
	// mislabel a valid result as partial.
	if v.UnclassifiedIncomeCount > 0 || v.UnclassifiedCashOutCount > 0 ||
		v.InvalidCashInClassificationCount > 0 || v.LegacyPurchaseCount > 0 ||
		v.LegacyRefundCount > 0 {
		out.Status = "incomplete"
	}
	return out
}

// PeriodWindow maps the FE-side period enum to a [from, to] range. Both ends
// inclusive; today is the upper bound for "today/week/month/3_months/year";
// last_month covers the prior calendar month. For PeriodCustom callers must
// use ResolveWindow with explicit from/to bounds.
func PeriodWindow(period string, today time.Time) (time.Time, time.Time) {
	t := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case PeriodToday:
		return t, t
	case PeriodWeek:
		return t.AddDate(0, 0, -6), t
	case PeriodLastMonth:
		first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
		last := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
		return first, last
	case Period3Months:
		return inclusiveRollingMonthsStart(t, 3), t
	case PeriodYear:
		return inclusiveRollingMonthsStart(t, 12), t
	default: // PeriodMonth
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), t
	}
}

// inclusiveRollingMonthsStart returns the day after the same calendar day N
// months ago, so both endpoints inclusive describe "the last N months"
// without counting the anchor twice. The anchor day is clamped to the target
// month's last day: May 31 minus three months anchors on February 28/29, not
// on a normalized March date as time.AddDate would do.
func inclusiveRollingMonthsStart(to time.Time, months int) time.Time {
	targetMonth := time.Date(to.Year(), to.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -months, 0)
	lastTargetDay := targetMonth.AddDate(0, 1, -1).Day()
	anchorDay := to.Day()
	if anchorDay > lastTargetDay {
		anchorDay = lastTargetDay
	}
	anchor := time.Date(targetMonth.Year(), targetMonth.Month(), anchorDay, 0, 0, 0, 0, time.UTC)
	return anchor.AddDate(0, 0, 1)
}

// ResolveWindow returns the inclusive [from, to] for any valid period. A
// custom range must contain both bounds in chronological order; callers get
// an explicit error instead of a silently substituted or reversed report.
func ResolveWindow(period string, from, to *time.Time, today time.Time) (time.Time, time.Time, error) {
	if err := validateReportWindow(period, from, to); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if period != PeriodCustom {
		f, t := PeriodWindow(period, today)
		return f, t, nil
	}
	f := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	en := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return f, en, nil
}

// previousWindow returns the same-length window immediately preceding [from,
// to]. For month/last_month it uses the prior calendar month so the
// comparison aligns to month boundaries; everything else uses a fixed-length
// shift ending the day before from.
func previousWindow(period string, from, to time.Time) (time.Time, time.Time) {
	switch period {
	case PeriodMonth, PeriodLastMonth:
		// Calendar-month alignment so "este mes" compares vs "mes pasado"
		// (both at month-end-to-date if mid-month).
		startOfMonth := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
		prevStart := startOfMonth.AddDate(0, -1, 0)
		// Same day-of-month offset as `to` (e.g. if comparing 1..14, compare
		// previous 1..14). Cap at last day of previous month.
		offset := int(to.Sub(from).Hours() / 24)
		prevEnd := prevStart.AddDate(0, 0, offset)
		lastOfPrev := startOfMonth.AddDate(0, 0, -1)
		if prevEnd.After(lastOfPrev) {
			prevEnd = lastOfPrev
		}
		return prevStart, prevEnd
	default:
		// Fixed-length shift: same number of days, ending the day before from.
		days := int(to.Sub(from).Hours()/24) + 1
		prevTo := from.AddDate(0, 0, -1)
		prevFrom := prevTo.AddDate(0, 0, -(days - 1))
		return prevFrom, prevTo
	}
}
