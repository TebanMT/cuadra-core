// HTTP-side wiring for the reports application layer (UC-033..UC-036) plus
// the persecución mutations (UC-035 mark contacted / mark lost). These all
// live in one controller because they share the same audience (owner) and
// the same auth surface; splitting would only add boilerplate.
package interfaces

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var (
	errBadID   = errors.New("id inválido")
	errBadAuth = errors.New("autenticación requerida")
)

// ReportsController bundles UC-033..UC-036 plus UC-035 persecución mutations.
type ReportsController struct {
	Dashboard         *reportsApp.Dashboard
	AttentionRequired *reportsApp.AttentionRequired
	Range             *reportsApp.RangeReport
	Export            *reportsApp.ExportReport
	MarkContacted     *memApp.MarkContacted
	MarkLost          *memApp.MarkLost
	Tokens            auth.TokenService
	// PlanGate (opcional) restringe la exportación de reportes a Plus. El
	// dashboard, atención inmediata y los reportes por rango siguen en
	// Standard — el export (CSV/Excel) es para contador / gym formal.
	PlanGate gin.HandlerFunc
}

func NewReportsController(
	dashboard *reportsApp.Dashboard,
	attention *reportsApp.AttentionRequired,
	rangeReport *reportsApp.RangeReport,
	export *reportsApp.ExportReport,
	markContacted *memApp.MarkContacted,
	markLost *memApp.MarkLost,
	tokens auth.TokenService,
) *ReportsController {
	return &ReportsController{
		Dashboard: dashboard, AttentionRequired: attention, Range: rangeReport,
		Export: export, MarkContacted: markContacted, MarkLost: markLost, Tokens: tokens,
	}
}

func (ctrl *ReportsController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		api.GET("/dashboard", ctrl.handleDashboard)
		api.GET("/attention-required", ctrl.handleAttentionRequired)
		api.GET("/reports", ctrl.handleRange)
		api.POST("/members/:id/contact-attempts", ctrl.handleMarkContacted)
		api.POST("/members/:id/mark-lost", ctrl.handleMarkLost)
		// Export de reportes (CSV/Excel) es Plus.
		plus := api.Group("")
		if ctrl.PlanGate != nil {
			plus.Use(ctrl.PlanGate)
		}
		plus.GET("/reports/:type/export", ctrl.handleExport)
	}
}

func (ctrl *ReportsController) handleRange(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	period := c.DefaultQuery("period", reportsApp.PeriodMonth)
	in := reportsApp.RangeReportInput{GymID: gymID, Period: period}
	if period == reportsApp.PeriodCustom {
		if v := c.Query("from"); v != "" {
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				utils.ErrorResponse(c, http.StatusBadRequest, err)
				return
			}
			in.From = &t
		}
		if v := c.Query("to"); v != "" {
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				utils.ErrorResponse(c, http.StatusBadRequest, err)
				return
			}
			in.To = &t
		}
	}
	out, err := ctrl.Range.Execute(c.Request.Context(), in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	// Augment with the two FE-only extras (recent_payments, attention_required_count).
	// Both come from the dashboard cache so we don't duplicate the heavy
	// Reader calls when the same gym is also viewing the dashboard.
	dash, _ := ctrl.Dashboard.Execute(c.Request.Context(), reportsApp.DashboardInput{GymID: gymID})
	utils.JsonResponse(c, http.StatusOK, toRangeWire(out, dash))
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type contactAttemptReq struct {
	Channel *string `json:"channel,omitempty"`
	Note    *string `json:"note,omitempty"`
}

type markLostReq struct {
	Reason string `json:"reason,omitempty"`
}

type contactAttemptResp struct {
	ContactAttemptID     uuid.UUID `json:"contact_attempt_id"`
	MemberID             uuid.UUID `json:"member_id"`
	LastContactAttemptAt time.Time `json:"last_contact_attempt_at"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (ctrl *ReportsController) handleDashboard(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	out, err := ctrl.Dashboard.Execute(c.Request.Context(), reportsApp.DashboardInput{GymID: gymID})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toDashboardWire(out))
}

// ---------------------------------------------------------------------------
// Wire DTOs — keep the FE contract independent of internal field names so we
// can rename the application-layer KPI struct (current/previous) without
// breaking the dashboard React app, which expects {value, delta, delta_pct}.
// ---------------------------------------------------------------------------

type kpiTrendWire struct {
	Value    float64  `json:"value"`
	Delta    *float64 `json:"delta"`
	DeltaPct *float64 `json:"delta_pct"`
}

type recentPaymentWire struct {
	ID            string  `json:"id"`
	MemberName    string  `json:"member_name"`
	Amount        float64 `json:"amount"`
	PaymentMethod string  `json:"payment_method"`
	PaymentDate   string  `json:"payment_date"`
	Concept       string  `json:"concept"`
}

type cashTodayWire struct {
	Total    float64            `json:"total"`
	ByMethod map[string]float64 `json:"by_method"`
}

type dashboardWire struct {
	GeneratedAt   time.Time    `json:"generated_at"`
	ActiveMembers kpiTrendWire `json:"active_members"`
	IncomeMonth   kpiTrendWire `json:"income_month"`
	// ExpensesMonth — agregado de mercancía + gastos generales. El FE
	// muestra UN solo número con hint "Mercancía + otros". Internamente
	// los sub-KPIs siguen existiendo en el use case por si después se
	// expone el desglose.
	ExpensesMonth    kpiTrendWire                `json:"expenses_month"`
	ExpiringWeek     kpiTrendWire                `json:"expiring_week"`
	Recoverable      kpiTrendWire                `json:"recoverable"`
	Income30d        []reportsApp.DailyIncome    `json:"income_30d"`
	AttentionSummary reportsApp.AttentionSummary `json:"attention_summary"`
	RecentPayments   []recentPaymentWire         `json:"recent_payments"`
	CashToday        cashTodayWire               `json:"cash_today"`
}

func toDashboardWire(d *reportsApp.DashboardOutput) dashboardWire {
	w := dashboardWire{
		GeneratedAt: d.GeneratedAt,
		ActiveMembers: kpiTrendWire{
			Value:    d.ActiveMembers.Current,
			Delta:    floatPtrIfMeaningful(d.ActiveMembers),
			DeltaPct: d.ActiveMembers.DeltaPct,
		},
		IncomeMonth: kpiTrendWire{
			Value:    d.IncomeMonth.Current,
			Delta:    floatPtrIfMeaningful(d.IncomeMonth),
			DeltaPct: d.IncomeMonth.DeltaPct,
		},
		ExpensesMonth: kpiTrendWire{
			Value:    d.ExpensesMonth.Current,
			Delta:    floatPtrIfMeaningful(d.ExpensesMonth),
			DeltaPct: d.ExpensesMonth.DeltaPct,
		},
		// ExpiringThisWeek and RecoverableExpired are stored as ints (no
		// previous-period snapshot in the use case yet). We promote to
		// KpiTrend with no delta — FE renders just the value.
		ExpiringWeek:     kpiTrendWire{Value: float64(d.ExpiringThisWeek)},
		Recoverable:      kpiTrendWire{Value: float64(d.RecoverableExpired)},
		Income30d:        nonNilDailyIncome(d.IncomeLast30Days),
		AttentionSummary: d.AttentionSummary,
		RecentPayments:   recentPaymentsToWire(d.RecentPayments),
		CashToday: cashTodayWire{
			Total:    d.TodayCashTotal,
			ByMethod: cashByMethod(d.TodayCash),
		},
	}
	return w
}

func floatPtrIfMeaningful(k reportsApp.KPI) *float64 {
	// The Dashboard use case always computes Delta = Current - Previous; we
	// only suppress it when both sides are zero (no signal either way).
	if k.Current == 0 && k.Previous == 0 {
		return nil
	}
	v := k.Delta
	return &v
}

func nonNilDailyIncome(in []reportsApp.DailyIncome) []reportsApp.DailyIncome {
	if in == nil {
		return []reportsApp.DailyIncome{}
	}
	return in
}

func recentPaymentsToWire(in []reportsApp.RecentPaymentRow) []recentPaymentWire {
	out := make([]recentPaymentWire, 0, len(in))
	for _, r := range in {
		name := ""
		if r.MemberName != nil {
			name = *r.MemberName
		}
		out = append(out, recentPaymentWire{
			ID:            r.ID.String(),
			MemberName:    name,
			Amount:        r.Amount,
			PaymentMethod: r.Method,
			PaymentDate:   r.PaymentDate.Format("2006-01-02"),
			Concept:       r.Concept,
		})
	}
	return out
}

// cashByMethod ensures the three canonical buckets are always present so the
// FE's `Record<PaymentMethod, number>` lookup never returns undefined.
func cashByMethod(src map[string]float64) map[string]float64 {
	out := map[string]float64{"cash": 0, "transfer": 0, "card": 0}
	for k, v := range src {
		out[k] = v
	}
	return out
}

func (ctrl *ReportsController) handleAttentionRequired(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	out, err := ctrl.AttentionRequired.Execute(c.Request.Context(), reportsApp.AttentionRequiredInput{GymID: gymID})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toAttentionWire(out))
}

// ---------------------------------------------------------------------------
// AttentionRequired wire DTOs — same rationale as the dashboard wire: the
// reports application-layer types use Go-style PascalCase fields (no JSON
// tags on the row structs) and FE-specific renames (days_left →
// days_until_expiry, payment_date → due_since, …) belong at this layer.
// ---------------------------------------------------------------------------

type expiringMemberWire struct {
	MemberID             string  `json:"member_id"`
	FullName             string  `json:"full_name"`
	Phone                string  `json:"phone"`
	ExpiryDate           string  `json:"expiry_date"`
	DaysUntilExpiry      int     `json:"days_until_expiry"`
	MembershipType       string  `json:"membership_type"`
	LastContactAttemptAt *string `json:"last_contact_attempt_at,omitempty"`
}

type expiredMemberWire struct {
	MemberID             string  `json:"member_id"`
	FullName             string  `json:"full_name"`
	Phone                string  `json:"phone"`
	ExpiryDate           string  `json:"expiry_date"`
	DaysOverdue          int     `json:"days_overdue"`
	DaysUntilExpiry      int     `json:"days_until_expiry"` // negative; FE inherits the field from Expiring
	MembershipType       string  `json:"membership_type"`
	ContactAttemptsCount int     `json:"contact_attempts_count"`
	LastContactAttemptAt *string `json:"last_contact_attempt_at,omitempty"`
}

type inactiveMemberWire struct {
	MemberID       string  `json:"member_id"`
	FullName       string  `json:"full_name"`
	Phone          string  `json:"phone"`
	LastVisitAt    *string `json:"last_visit_at"`
	DaysSinceVisit int     `json:"days_since_visit"`
}

type lowStockProductWire struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	Stock     int    `json:"stock"`
	MinStock  int    `json:"min_stock"`
}

type pendingBalanceWire struct {
	MemberID string  `json:"member_id"`
	FullName string  `json:"full_name"`
	Phone    string  `json:"phone"`
	Balance  float64 `json:"balance"`
	DueSince string  `json:"due_since"`
}

type birthdayWire struct {
	MemberID string `json:"member_id"`
	FullName string `json:"full_name"`
	Phone    string `json:"phone"`
	Age      int    `json:"age"`
}

type attentionWire struct {
	GeneratedAt         time.Time             `json:"generated_at"`
	ExpiringSoon        []expiringMemberWire  `json:"expiring_soon"`
	ExpiredRecoverable  []expiredMemberWire   `json:"expired_recoverable"`
	InactiveInvoluntary []inactiveMemberWire  `json:"inactive_involuntary"`
	LowStock            []lowStockProductWire `json:"low_stock"`
	PendingBalance      []pendingBalanceWire  `json:"pending_balance"`
	BirthdaysToday      []birthdayWire        `json:"birthdays_today"`
}

func toAttentionWire(d *reportsApp.AttentionRequiredOutput) attentionWire {
	now := d.GeneratedAt
	w := attentionWire{
		GeneratedAt:         d.GeneratedAt,
		ExpiringSoon:        make([]expiringMemberWire, 0, len(d.ExpiringSoon)),
		ExpiredRecoverable:  make([]expiredMemberWire, 0, len(d.RecoverableExpired)),
		InactiveInvoluntary: make([]inactiveMemberWire, 0, len(d.InactiveInvoluntary)),
		LowStock:            make([]lowStockProductWire, 0, len(d.LowStock)),
		PendingBalance:      make([]pendingBalanceWire, 0, len(d.PendingBalances)),
		BirthdaysToday:      make([]birthdayWire, 0, len(d.BirthdaysToday)),
	}
	for _, m := range d.ExpiringSoon {
		w.ExpiringSoon = append(w.ExpiringSoon, expiringMemberWire{
			MemberID:        m.MemberID.String(),
			FullName:        m.FullName,
			Phone:           m.Phone,
			ExpiryDate:      m.ExpiryDate.Format("2006-01-02"),
			DaysUntilExpiry: m.DaysLeft,
			MembershipType:  m.MembershipType,
		})
	}
	for _, m := range d.RecoverableExpired {
		w.ExpiredRecoverable = append(w.ExpiredRecoverable, expiredMemberWire{
			MemberID:             m.MemberID.String(),
			FullName:             m.FullName,
			Phone:                m.Phone,
			ExpiryDate:           m.ExpiryDate.Format("2006-01-02"),
			DaysOverdue:          m.DaysOverdue,
			DaysUntilExpiry:      -m.DaysOverdue,
			MembershipType:       m.MembershipType,
			ContactAttemptsCount: m.ContactAttemptsCount,
			LastContactAttemptAt: timePtrToISO(m.LastContactAttemptAt),
		})
	}
	for _, m := range d.InactiveInvoluntary {
		w.InactiveInvoluntary = append(w.InactiveInvoluntary, inactiveMemberWire{
			MemberID:       m.MemberID.String(),
			FullName:       m.FullName,
			Phone:          m.Phone,
			LastVisitAt:    timePtrToISO(m.LastCheckinAt),
			DaysSinceVisit: m.DaysAbsent,
		})
	}
	for _, p := range d.LowStock {
		w.LowStock = append(w.LowStock, lowStockProductWire{
			ProductID: p.ProductID.String(),
			Name:      p.Name,
			Stock:     p.Stock,
			MinStock:  p.StockMinimum,
		})
	}
	for _, b := range d.PendingBalances {
		w.PendingBalance = append(w.PendingBalance, pendingBalanceWire{
			MemberID: b.MemberID.String(),
			FullName: b.FullName,
			Phone:    b.Phone,
			Balance:  b.BalancePending,
			DueSince: b.PaymentDate.Format("2006-01-02"),
		})
	}
	for _, b := range d.BirthdaysToday {
		w.BirthdaysToday = append(w.BirthdaysToday, birthdayWire{
			MemberID: b.MemberID.String(),
			FullName: b.FullName,
			Phone:    b.Phone,
			Age:      yearsBetween(b.Birthdate, now),
		})
	}
	return w
}

func timePtrToISO(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

// rangeWire mirrors the FE ReportsRangeData shape — superset of the
// application-layer RangeReportOutput. Each totals.* surfaces as a
// {value, delta, delta_pct} so StatCards can render trend chips. The two
// FE-only extras (recent_payments, attention_required_count) come from the
// dashboard's cached widget.
type rangeWire struct {
	Period                 string                         `json:"period"`
	From                   string                         `json:"from"`
	To                     string                         `json:"to"`
	Totals                 rangeTotalsWire                `json:"totals"`
	IncomeByDay            []reportsApp.DailyIncome       `json:"income_by_day"`
	ExpensesByDay          []reportsApp.DailyAmount       `json:"expenses_by_day"`
	CheckinsByDay          []reportsApp.DailyCount        `json:"checkins_by_day"`
	IncomeByMethod         map[string]float64             `json:"income_by_method"`
	ExpensesByCategory     map[string]float64             `json:"expenses_by_category"`
	TopMembers             []reportsApp.TopMemberRow      `json:"top_members"`
	TopProducts            []reportsApp.TopProductRow     `json:"top_products"`
	InventoryCosts         []inventoryCostWire            `json:"inventory_costs"`
	Expenses               []expenseWire                  `json:"expenses"`
	CriticalStock          reportsApp.CriticalStockCounts `json:"critical_stock"`
	RecentPayments         []recentPaymentWire            `json:"recent_payments"`
	AttentionRequiredCount int                            `json:"attention_required_count"`
}

// rangeTotalsWire — cada total como KPI trend para que el FE renderee
// deltas en los StatCards.
type rangeTotalsWire struct {
	Income          kpiTrendWire `json:"income"`
	InventoryCost   kpiTrendWire `json:"inventory_cost"`
	ExpensesGeneral kpiTrendWire `json:"expenses_general"`
	Refunds         kpiTrendWire `json:"refunds"`
	NewMembers      kpiTrendWire `json:"new_members"`
	Checkins        kpiTrendWire `json:"checkins"`
	Net             kpiTrendWire `json:"net"`
}

// expenseWire — fila de la tabla "Gastos del período". UUID y date van
// como string. PaymentMethod usa los mismos buckets que payments
// (cash/transfer/card).
type expenseWire struct {
	ID            string  `json:"id"`
	ExpenseDate   string  `json:"expense_date"`
	Amount        float64 `json:"amount"`
	Category      string  `json:"category"`
	Description   *string `json:"description,omitempty"`
	PaymentMethod string  `json:"payment_method"`
}

func expensesToWire(in []reportsApp.ExpenseRow) []expenseWire {
	out := make([]expenseWire, 0, len(in))
	for _, r := range in {
		out = append(out, expenseWire{
			ID:            r.ID.String(),
			ExpenseDate:   r.ExpenseDate.Format("2006-01-02"),
			Amount:        r.Amount,
			Category:      r.Category,
			Description:   r.Description,
			PaymentMethod: r.PaymentMethod,
		})
	}
	return out
}

// inventoryCostWire — fila de la tabla "Compras de inventario". Las
// fechas y UUIDs van como string para evitar idiosincrasias del FE.
type inventoryCostWire struct {
	MovementID  string  `json:"movement_id"`
	ProductID   string  `json:"product_id"`
	ProductName string  `json:"product_name"`
	Delta       int     `json:"delta"`
	CostUnit    float64 `json:"cost_unit"`
	CostTotal   float64 `json:"cost_total"`
	Reason      *string `json:"reason,omitempty"`
	OccurredAt  string  `json:"occurred_at"`
}

func inventoryCostsToWire(in []reportsApp.InventoryCostRow) []inventoryCostWire {
	out := make([]inventoryCostWire, 0, len(in))
	for _, r := range in {
		out = append(out, inventoryCostWire{
			MovementID:  r.MovementID.String(),
			ProductID:   r.ProductID.String(),
			ProductName: r.ProductName,
			Delta:       r.Delta,
			CostUnit:    r.CostUnit,
			CostTotal:   r.CostTotal,
			Reason:      r.Reason,
			OccurredAt:  r.OccurredAt.Format(time.RFC3339),
		})
	}
	return out
}

func toRangeWire(r *reportsApp.RangeReportOutput, dash *reportsApp.DashboardOutput) rangeWire {
	w := rangeWire{
		Period: r.Period,
		From:   r.From,
		To:     r.To,
		Totals: rangeTotalsWire{
			Income:          kpiToWire(r.Totals.Income),
			InventoryCost:   kpiToWire(r.Totals.InventoryCost),
			ExpensesGeneral: kpiToWire(r.Totals.ExpensesGeneral),
			Refunds:         kpiToWire(r.Totals.Refunds),
			NewMembers:      kpiToWire(r.Totals.NewMembers),
			Checkins:        kpiToWire(r.Totals.Checkins),
			Net:             kpiToWire(r.Totals.Net),
		},
		IncomeByDay:        nonNilDailyIncome(r.IncomeByDay),
		ExpensesByDay:      nonNilDailyAmount(r.ExpensesByDay),
		CheckinsByDay:      nonNilDailyCount(r.CheckinsByDay),
		IncomeByMethod:     cashByMethod(r.IncomeByMethod),
		ExpensesByCategory: nonNilCategoryMap(r.ExpensesByCategory),
		TopMembers:         nonNilTopMembers(r.TopMembers),
		TopProducts:        nonNilTopProducts(r.TopProducts),
		InventoryCosts:     inventoryCostsToWire(r.InventoryCosts),
		Expenses:           expensesToWire(r.Expenses),
		CriticalStock:      r.CriticalStock,
		RecentPayments:     []recentPaymentWire{},
	}
	if dash != nil {
		w.RecentPayments = recentPaymentsToWire(dash.RecentPayments)
		s := dash.AttentionSummary
		w.AttentionRequiredCount = s.ExpiringSoon + s.ExpiredRecoverable + s.InactiveInvoluntary +
			s.LowStock + s.PendingBalance + s.BirthdaysToday
	}
	return w
}

// kpiToWire flattens the application-layer KPI into the FE-friendly
// {value, delta, delta_pct} shape. Same convention dashboard uses.
func kpiToWire(k reportsApp.KPI) kpiTrendWire {
	return kpiTrendWire{
		Value:    k.Current,
		Delta:    floatPtrIfMeaningful(k),
		DeltaPct: k.DeltaPct,
	}
}

func nonNilDailyAmount(in []reportsApp.DailyAmount) []reportsApp.DailyAmount {
	if in == nil {
		return []reportsApp.DailyAmount{}
	}
	return in
}

func nonNilCategoryMap(in map[string]float64) map[string]float64 {
	if in == nil {
		return map[string]float64{}
	}
	return in
}

func nonNilTopProducts(in []reportsApp.TopProductRow) []reportsApp.TopProductRow {
	if in == nil {
		return []reportsApp.TopProductRow{}
	}
	return in
}

func nonNilDailyCount(in []reportsApp.DailyCount) []reportsApp.DailyCount {
	if in == nil {
		return []reportsApp.DailyCount{}
	}
	return in
}

func nonNilTopMembers(in []reportsApp.TopMemberRow) []reportsApp.TopMemberRow {
	if in == nil {
		return []reportsApp.TopMemberRow{}
	}
	return in
}

// yearsBetween returns whole years between birthdate and today, accounting
// for whether the birthday has passed this year.
func yearsBetween(birth, today time.Time) int {
	y := today.Year() - birth.Year()
	bdayThisYear := time.Date(today.Year(), birth.Month(), birth.Day(), 0, 0, 0, 0, today.Location())
	if today.Before(bdayThisYear) {
		y--
	}
	return y
}

func (ctrl *ReportsController) handleExport(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	reportType := c.Param("type")
	format := c.DefaultQuery("format", reportsApp.FormatPDF)

	in := reportsApp.ExportInput{
		GymID:  gymID,
		Type:   reportType,
		Format: format,
		Period: c.Query("period"),
	}
	if v := c.Query("from"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		in.From = &t
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		in.To = &t
	}
	out, err := ctrl.Export.Execute(c.Request.Context(), in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+out.Filename+`"`)
	c.Header("Content-Length", strconv.Itoa(len(out.Bytes)))
	c.Data(http.StatusOK, out.ContentType, out.Bytes)
}

func (ctrl *ReportsController) handleMarkContacted(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req contactAttemptReq
	_ = c.ShouldBindJSON(&req)
	out, err := ctrl.MarkContacted.Execute(c.Request.Context(), memApp.MarkContactedInput{
		GymID: gymID, ActorUserID: userID, MemberID: id,
		Channel: req.Channel, Note: req.Note,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	ctrl.Dashboard.InvalidateCache(gymID)
	utils.JsonResponse(c, http.StatusCreated, contactAttemptResp{
		ContactAttemptID:     out.ContactAttemptID,
		MemberID:             out.MemberID,
		LastContactAttemptAt: out.LastContactAttemptAt,
	})
}

func (ctrl *ReportsController) handleMarkLost(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req markLostReq
	_ = c.ShouldBindJSON(&req)
	out, err := ctrl.MarkLost.Execute(c.Request.Context(), memApp.MarkLostInput{
		GymID: gymID, ActorUserID: userID, MemberID: id, Reason: req.Reason,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	ctrl.Dashboard.InvalidateCache(gymID)
	utils.JsonResponse(c, http.StatusOK, gin.H{
		"member_id": out.ID,
		"status":    out.Status,
	})
}

func parseUUIDParam(c *gin.Context, name string) (uuid.UUID, bool) {
	raw := c.Param(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return uuid.Nil, false
	}
	return id, true
}
