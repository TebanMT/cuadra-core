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

type RangeReportInput struct {
	GymID  uuid.UUID
	Period string
	// From/To only consulted when Period == PeriodCustom. Inclusive, day-grain.
	From *time.Time
	To   *time.Time
}

// RangeReportOutput matches the FE ReportsRangeData shape.
type RangeReportOutput struct {
	Period string      `json:"period"`
	From   string      `json:"from"`
	To     string      `json:"to"`
	Totals RangeTotals `json:"totals"`
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
	// InventoryCosts — movimientos restock con costo del período.
	InventoryCosts []InventoryCostRow `json:"inventory_costs"`
	// Expenses — gastos generales (BC expenses) del período.
	Expenses []ExpenseRow `json:"expenses"`
	// CriticalStock — snapshot actual de stock (no varía con período).
	CriticalStock CriticalStockCounts `json:"critical_stock"`
}

// RangeTotals is the FE-facing totals block. Each numeric value carries a
// previous-window comparison so the StatCards can render deltas. Net is
// derived: income − inventory_cost − expenses_general − refunds (both current
// and previous so the trend chip is meaningful). Refunds entran al neto
// porque Income es BRUTO (SumPaymentsBetween excluye concept='refund') — sin
// restarlas, la "utilidad" del período ignoraba el dinero devuelto.
type RangeTotals struct {
	Income          KPI `json:"income"`
	InventoryCost   KPI `json:"inventory_cost"`
	ExpensesGeneral KPI `json:"expenses_general"`
	Refunds         KPI `json:"refunds"`
	NewMembers      KPI `json:"new_members"`
	Checkins        KPI `json:"checkins"`
	Net             KPI `json:"net"`
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
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	// El "hoy" del período es el día del GYM, no el de UTC — desde las 6 PM
	// de CDMX "Hoy" resolvía al día siguiente y salía en ceros aunque hubiera
	// cobros. Mismo anclaje que el dashboard (localToday).
	today, tzName := localTodayAndTZ(tx, uc.Gyms, in.GymID, uc.now())
	from, to := ResolveWindow(in.Period, in.From, in.To, today)
	prevFrom, prevTo := previousWindow(in.Period, from, to)

	out := &RangeReportOutput{
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
	}

	// KPI scalar reads — current + previous window. Errors degrade silently
	// to zero per the use case's existing posture: the page is composed of
	// independent cards, so one broken sub-query shouldn't blank everything.
	incomeNow, _ := uc.Reader.SumPaymentsBetween(tx, in.GymID, from, to)
	incomePrev, _ := uc.Reader.SumPaymentsBetween(tx, in.GymID, prevFrom, prevTo)
	out.Totals.Income = newKPI(incomeNow, incomePrev)

	inventoryNow, _ := uc.Reader.SumInventoryCostBetween(tx, in.GymID, tzName, from, to)
	inventoryPrev, _ := uc.Reader.SumInventoryCostBetween(tx, in.GymID, tzName, prevFrom, prevTo)
	out.Totals.InventoryCost = newKPI(inventoryNow, inventoryPrev)

	expensesNow, _ := uc.Reader.SumExpensesBetween(tx, in.GymID, from, to)
	expensesPrev, _ := uc.Reader.SumExpensesBetween(tx, in.GymID, prevFrom, prevTo)
	out.Totals.ExpensesGeneral = newKPI(expensesNow, expensesPrev)

	refundsNow, _ := uc.Reader.SumRefundsBetween(tx, in.GymID, from, to)
	refundsPrev, _ := uc.Reader.SumRefundsBetween(tx, in.GymID, prevFrom, prevTo)
	out.Totals.Refunds = newKPI(refundsNow, refundsPrev)

	newMembersNow, _ := uc.Reader.CountNewMembersBetween(tx, in.GymID, tzName, from, to)
	newMembersPrev, _ := uc.Reader.CountNewMembersBetween(tx, in.GymID, tzName, prevFrom, prevTo)
	out.Totals.NewMembers = newKPI(float64(newMembersNow), float64(newMembersPrev))

	checkinsNow, _ := uc.Reader.CountCheckinsBetween(tx, in.GymID, tzName, from, to)
	checkinsPrev, _ := uc.Reader.CountCheckinsBetween(tx, in.GymID, tzName, prevFrom, prevTo)
	out.Totals.Checkins = newKPI(float64(checkinsNow), float64(checkinsPrev))

	productNow, _ := uc.Reader.SumProductSalesBetween(tx, in.GymID, from, to)
	productPrev, _ := uc.Reader.SumProductSalesBetween(tx, in.GymID, prevFrom, prevTo)
	out.ProductSales = ProductSalesKPI{
		Amount: newKPI(productNow.Amount, productPrev.Amount),
		Units:  productNow.Units,
	}

	// Net = income − inventory_cost − expenses_general − refunds. Computed on
	// both windows so the FE can show a meaningful trend chip. Income es bruto
	// (excluye refunds) y SumRefundsBetween devuelve el valor ABSOLUTO — se
	// resta, no se suma.
	netNow := incomeNow - inventoryNow - expensesNow - refundsNow
	netPrev := incomePrev - inventoryPrev - expensesPrev - refundsPrev
	out.Totals.Net = newKPI(netNow, netPrev)

	// Daily series (charts).
	if series, err := uc.Reader.IncomeDailySeries(tx, in.GymID, from, to); err == nil && series != nil {
		out.IncomeByDay = series
	}
	if rows, err := uc.Reader.ExpensesDailySeries(tx, in.GymID, tzName, from, to); err == nil && rows != nil {
		out.ExpensesByDay = rows
	}
	if rows, err := uc.Reader.CheckinsDailySeries(tx, in.GymID, tzName, from, to); err == nil && rows != nil {
		out.CheckinsByDay = rows
	}

	// Breakdowns + leaderboards.
	if m, err := uc.Reader.IncomeByMethodBetween(tx, in.GymID, from, to); err == nil && m != nil {
		out.IncomeByMethod = m
	}
	if m, err := uc.Reader.ExpensesByCategoryBetween(tx, in.GymID, from, to); err == nil && m != nil {
		out.ExpensesByCategory = m
	}
	if m, err := uc.Reader.IncomeByMembershipTypeBetween(tx, in.GymID, from, to); err == nil && m != nil {
		out.IncomeByMembershipType = m
	}
	if m, err := uc.Reader.ActiveMembersByType(tx, in.GymID, today); err == nil && m != nil {
		out.MembersByType = m
	}
	if rows, err := uc.Reader.TopMembersBetween(tx, in.GymID, from, to, topMembersLimit); err == nil && rows != nil {
		out.TopMembers = rows
	}
	if rows, err := uc.Reader.TopProductsBetween(tx, in.GymID, from, to, topProductsLimit); err == nil && rows != nil {
		out.TopProducts = rows
	}

	// Tablas detalladas del período.
	if rows, err := uc.Reader.ListInventoryCostsBetween(tx, in.GymID, tzName, from, to, 200); err == nil && rows != nil {
		out.InventoryCosts = rows
	}
	if rows, err := uc.Reader.ListExpensesBetween(tx, in.GymID, from, to, 200); err == nil && rows != nil {
		out.Expenses = rows
	}

	// Snapshot de stock crítico (no varía con período).
	if c, err := uc.Reader.CountCriticalStock(tx, in.GymID); err == nil {
		out.CriticalStock = c
	}

	return out, nil
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
		return t.AddDate(0, -3, 0), t
	case PeriodYear:
		return t.AddDate(-1, 0, 0), t
	default: // PeriodMonth
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), t
	}
}

// ResolveWindow returns the inclusive [from, to] for any period including
// "custom". For custom, both bounds come from the caller; nil/zero fall back
// to "today" so a malformed request still renders something.
func ResolveWindow(period string, from, to *time.Time, today time.Time) (time.Time, time.Time) {
	if period != PeriodCustom {
		return PeriodWindow(period, today)
	}
	t := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	f := t
	if from != nil && !from.IsZero() {
		f = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	}
	en := t
	if to != nil && !to.IsZero() {
		en = time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	}
	if en.Before(f) {
		// Swap silently — the FE shouldn't get a 400 just because a user
		// flipped the pickers.
		f, en = en, f
	}
	return f, en
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
