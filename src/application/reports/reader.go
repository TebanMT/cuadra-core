// Reader is the read-only seam between the reports application layer and the
// raw SQL aggregations that back UC-033..UC-036. The interface lives next to
// the use cases (cross-cutting reads, no BC) and the implementation lives in
// infraestructure/queries_postgres.go (cloud) — analogous to how billing
// CashCloseReader is split.
//
// All methods take a sharedDomain.Transaction so they can run inside the same
// UoW.Query() handle the use case opens. None of them write — they're meant
// to compose freely behind the dashboard cache.
package reports

import (
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CashCountSummary describes the physical counts captured in completed cash
// closes. CountedCloses can be lower than TotalCloses because the API permits
// closing without a count; callers must not calculate a reconciliation from
// an incomplete set.
type CashCountSummary struct {
	Counted       float64
	CountedCloses int
	TotalCloses   int
	Withdrawn     float64
	// Session counters form a partition over tracked, non-deleted sessions in the
	// requested operational-date range. TotalCloses intentionally excludes
	// OpenSessions; TotalSessions includes them.
	// Dates and legacy cuts before the first explicit opening are history,
	// not missing work. Activity totals still include those dates.
	HistoricalActivityDays          int
	HistoricalSessions              int
	ActiveDays                      int
	MissingActiveDays               int
	UncoveredActivityDays           int
	OpenSessions                    int
	ClosedUnverifiedSessions        int
	ReconciledSessions              int
	StaleSessions                   int
	WithdrawnSessions               int
	UnknownOpeningSessions          int
	AdjustedAfterWithdrawalSessions int
	// See CanonicalFinancialSnapshot.LegacyCashSourceUnverifiedCount. These
	// rows are deliberately excluded from physical activity until repaired.
	LegacyCashSourceUnverifiedCount int
	TotalSessions                   int
	LatestSessionID                 *uuid.UUID
	LatestExpected                  *float64
	LatestCounted                   *float64
	LatestDifference                *float64
	LatestCountedAt                 *time.Time
	LatestStatus                    string
	LatestNeedsRecount              bool
}

// CanonicalFinancialSnapshot is the single source for the Standard business
// summary. Values are cash-basis period amounts; counts preserve uncertainty
// instead of silently converting incomplete legacy data into zero.
type CanonicalFinancialSnapshot struct {
	MembershipIncome                 float64
	ProductIncome                    float64
	OtherIncome                      float64
	UnclassifiedIncome               float64
	OperatingExpenses                float64
	InventoryPurchases               float64
	Refunds                          float64
	UnclassifiedIncomeCount          int
	UnclassifiedCashOutCount         int
	InvalidCashInClassificationCount int
	// LegacyCashSourceUnverifiedCount counts historical cash-method expenses
	// whose migration guessed "cash register" but did not create a linked
	// physical movement. They remain valid operating expenses; only their
	// effect on the drawer is unknown until an owner confirms Caja or Fondo.
	LegacyCashSourceUnverifiedCount int
	LegacyPurchaseCount             int
	LegacyRefundCount               int
}

// CanonicalFinancialReader is optional for compatibility with old/fake
// readers. PostgreSQL and SQLite implement it; RangeReport uses a transparent
// legacy fallback only in tests/one-release adapters.
type CanonicalFinancialReader interface {
	CanonicalFinancialBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (CanonicalFinancialSnapshot, error)
}

// FinancialReadMetadata lets clients distinguish "calculated now" from
// "the newest financial mutation included in this snapshot". SyncPending is
// intentionally nullable: SQLite can answer it from its local queue, while
// the cloud cannot know whether a particular desktop still has unsent work.
type FinancialReadMetadata struct {
	DataWatermark *time.Time
	SyncPending   *bool
}

type FinancialMetadataReader interface {
	FinancialReadMetadata(tx sharedDomain.Transaction, gymID uuid.UUID) (FinancialReadMetadata, error)
}

type ProductProfitabilityReader interface {
	ProductProfitabilityBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]ProductProfitabilityRow, error)
}

type ProductProfitabilityRow struct {
	ProductID    uuid.UUID `json:"product_id"`
	ProductName  string    `json:"product_name"`
	Quantity     int       `json:"quantity"`
	Revenue      float64   `json:"revenue"`
	COGS         float64   `json:"cogs"`
	GrossProfit  float64   `json:"gross_profit"`
	MarginPct    *float64  `json:"margin_pct,omitempty"`
	CostComplete bool      `json:"cost_complete"`
}

// Reader is the cross-context query surface for reports.
type Reader interface {
	// Dashboard KPIs (UC-033)
	CountActiveMembers(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (int, error)
	SumPaymentsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error)
	// SumOtherIncomeBetween is the subset of income captured with concept
	// "other" (sale of old equipment, one-off income, etc.).
	SumOtherIncomeBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error)
	// SumCashClosedBetween is the legacy method name for the complete physical
	// cash activity in the period: cash payments (refunds are negative) plus
	// cash-in minus cash-out movements. It intentionally includes activity with
	// no close, an open session, or activity after a withdrawal; session coverage
	// reports whether that activity has been counted and certified.
	SumCashClosedBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error)
	// SumCashCountedBetween returns the physical cash recorded in the closes
	// plus coverage, so reports only show a difference when every close in the
	// selected period has a physical count.
	SumCashCountedBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (CashCountSummary, error)
	CountExpiringBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (int, error)
	CountExpiredRecoverable(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, withinDays int) (int, error)
	TodayCashByMethod(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (map[string]float64, error)
	IncomeDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]DailyIncome, error)

	// Attention-required lists (UC-034)
	ListExpiringSoon(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, days int) ([]MemberExpiringRow, error)
	ListExpiredRecoverable(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, withinDays int, staleContactDays int) ([]MemberExpiredRow, error)
	ListInactiveInvoluntary(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, daysWithoutCheckin int) ([]MemberInactiveRow, error)
	ListLowStock(tx sharedDomain.Transaction, gymID uuid.UUID) ([]ProductLowStockRow, error)
	ListPendingBalances(tx sharedDomain.Transaction, gymID uuid.UUID) ([]PendingBalanceRow, error)
	ListBirthdaysOn(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) ([]MemberBirthdayRow, error)

	// Export feeders (UC-036)
	ListMembersForExport(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) ([]MemberExportRow, error)
	ListPaymentsForExport(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]PaymentExportRow, error)
	ListSalesForExport(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) ([]SaleExportRow, error)

	// Range report extras (UC-036 — totals + breakdowns over an arbitrary
	// window; complement the dashboard KPIs).
	//
	// CONVENCIÓN tzName: los métodos que filtran una columna de TIMESTAMP
	// (checkin_at, created_at) reciben la zona del gym además del rango de
	// días. Sin ella, la base agrupa los instantes por día UTC y en CDMX
	// todo lo posterior a las 6 PM — el horario pico del gym — se archiva
	// en el día siguiente. Los que filtran columnas DATE (payment_date,
	// expense_date) NO la necesitan: esas ya se escriben en el día local.
	// Zona vacía = UTC (fail-open). Ver tz.DayBounds.
	CountNewMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (int, error)
	CountCheckinsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (int, error)
	SumRefundsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error)
	IncomeByMethodBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (map[string]float64, error)

	// IncomeByMembershipTypeBetween — cobros de membresía (concept =
	// 'membership', cash-based por payment_date, mismo criterio que el KPI
	// de ingresos) agrupados por el tipo de la membresía que el pago
	// renovó. Los pagos no llevan tipo: se atribuye vía la membresía del
	// socio con start_date más reciente <= payment_date (el pago crea la
	// membresía el mismo día, así que empatan; renovaciones anticipadas de
	// OTRO tipo pueden atribuirse al tipo anterior — aproximación
	// documentada). Abonos y ventas NO entran. Breakdown Standard.
	IncomeByMembershipTypeBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (map[string]float64, error)

	// ActiveMembersByType — snapshot de socios ACTIVOS agrupados por el
	// tipo de su membresía vigente (mismo predicado que CountActiveMembers,
	// para que la suma de los buckets cuadre con ese KPI). No depende del
	// período. Breakdown Standard.
	ActiveMembersByType(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (map[string]int, error)
	TopMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]TopMemberRow, error)
	CheckinsDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) ([]DailyCount, error)

	// ExpensesDailySeries totaliza por día gastos generales pagados,
	// compras explícitas de inventario pagadas y devoluciones reales de
	// ingreso. Todas usan su fecha económica local (expense_date, paid_on o
	// refunded_on), de modo que la serie cuadra con los egresos canónicos.
	// tzName se conserva en la interfaz por compatibilidad entre lectores.
	ExpensesDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) ([]DailyAmount, error)

	// ExpensesByCategoryBetween devuelve el total de gastos generales
	// agrupado por categoría del enum (renta, servicios, …). Sólo
	// considera BC expenses; las compras de mercancía no entran (tienen
	// su propia sección "Compras de inventario").
	ExpensesByCategoryBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (map[string]float64, error)

	// TopProductsBetween — ranking por revenue (price * quantity) sobre
	// sale_items en pagos del período. Filtra por payment_date para
	// alinear con el resto de las queries por ventana.
	TopProductsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]TopProductRow, error)

	// SumProductSalesBetween — $ y unidades de la venta de productos del
	// período: $ = SUM(amount) de payments con concept='product' (cash-based
	// por payment_date, mismo criterio que SumPaymentsBetween, para que
	// membresías-por-tipo + productos + abonos ≈ Ingresos); unidades =
	// SUM(quantity) de los sale_items de esos mismos pagos. payment_date es
	// DATE → sin tzName. Los refunds de venta NO restan aquí — viven en el
	// KPI de devoluciones, igual que en el resto de los breakdowns.
	SumProductSalesBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (ProductSalesTotals, error)

	// CountCriticalStock — snapshot del catálogo: cuántos productos
	// activos están a 0 (out) y cuántos por debajo del mínimo pero >0
	// (low). NO depende de período — es estado actual.
	CountCriticalStock(tx sharedDomain.Transaction, gymID uuid.UUID) (CriticalStockCounts, error)

	// ListRecentPayments returns the latest non-refund payments for a gym,
	// ordered by payment_date DESC then created_at DESC. Used by the
	// dashboard's "últimos cobros" widget.
	ListRecentPayments(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]RecentPaymentRow, error)

	// SumInventoryCostBetween totaliza las compras explícitas de inventario
	// efectivamente pagadas en un rango, según paid_on. Un ajuste de stock no
	// es por sí mismo una salida de dinero. Usado por:
	//   - Dashboard (KPI inventory_cost_month, current + previous)
	//   - RangeReport (totals.inventory_cost del período seleccionado)
	SumInventoryCostBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (float64, error)

	// RealizedProductProfitBetween — ganancia REALIZADA Plus: ingreso de
	// productos reconocido en caja menos COGS reconocido proporcionalmente.
	// El costo unitario es el snapshot congelado en sale_items al vender; una
	// compra posterior nunca reescribe el margen histórico. Los productos con
	// alguna línea sin snapshot siguen en revenue y se declaran incompletos.
	RealizedProductProfitBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (RealizedProductProfit, error)

	// ListInventoryCostsBetween lista las compras explícitas pagadas en un
	// rango, ordenadas por paid_on DESC. JOINea
	// product_name para que el FE no tenga que hacer N+1. Usado por la
	// tabla "Compras de inventario" en la página de reportes.
	ListInventoryCostsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time, limit int) ([]InventoryCostRow, error)

	// SumExpensesBetween totaliza los gastos generales (BC expenses) en
	// un rango. Filtra por expense_date — el campo del usuario, no el
	// created_at del row — para que "egresos del mes" refleje cuándo
	// pasó el gasto, no cuándo lo capturó el operador.
	SumExpensesBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error)

	// ListExpensesBetween lista los gastos del rango ordenados por
	// expense_date DESC (más reciente arriba). Alimenta la tabla
	// "Gastos del período" en reportes.
	ListExpensesBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]ExpenseRow, error)

	// GenderComposition cuenta socios activos por bucket de género. NULL
	// en members.gender se proyecta a no_especificado para que el FE
	// pueda graficar 3 segmentos sin tener que merge-ear "null" en cliente.
	// Activos = m.status='active' AND membership activa con expiry >=
	// today (mismo criterio que CountActiveMembers).
	GenderComposition(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (GenderCompositionRow, error)

	// AttendanceByGenderHour devuelve, para los últimos `daysBack` días,
	// el conteo de check-ins exitosos por hora del día (0-23) cruzado con
	// género del socio. NULL → no_especificado. Cada hora del día aparece
	// exactamente una vez en la salida (24 filas) aunque no haya tráfico
	// (zeros explícitos), para que el FE renderee la grilla completa sin
	// reconciliar gaps. La hora es la LOCAL del gym (tzName) — con la hora
	// UTC el heatmap salía corrido 6 horas en CDMX (el pico de las 7 PM
	// aparecía en la 1 AM). Zona vacía = UTC (fail-open).
	AttendanceByGenderHour(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, daysBack int, now time.Time) ([]AttendanceByGenderHourRow, error)
}

// GenderCompositionRow es el agregado total + 3 buckets para el donut. El
// FE calcula porcentajes (Total puede ser 0 y queremos evitar division
// silenciosa en el server).
type GenderCompositionRow struct {
	Hombre         int `json:"hombre"`
	Mujer          int `json:"mujer"`
	NoEspecificado int `json:"no_especificado"`
	Total          int `json:"total"`
}

// AttendanceByGenderHourRow — una fila por hora (0..23) con conteos por
// bucket. JSON tags para emitir tal cual desde el controller sin DTO
// adicional (mismo patrón que DailyIncome).
type AttendanceByGenderHourRow struct {
	Hour           int `json:"hour"`
	Hombre         int `json:"hombre"`
	Mujer          int `json:"mujer"`
	NoEspecificado int `json:"no_especificado"`
}

// ExpenseRow — una fila de la tabla "Gastos del período". Refleja la
// shape de expenses sin sobre-exponer (no incluye created_by ni
// version). Description es opcional, igual que en el dominio.
type ExpenseRow struct {
	ID            uuid.UUID
	ExpenseDate   time.Time
	Amount        float64
	Category      string
	Description   *string
	PaymentMethod string
}

// InventoryCostRow — una compra explícita pagada. CostUnit es unitario y
// CostTotal = CostUnit * Delta. OccurredAt representa paid_on; MovementID
// conserva el vínculo auditable con la entrada de stock.
type InventoryCostRow struct {
	MovementID  uuid.UUID
	ProductID   uuid.UUID
	ProductName string
	Delta       int     // unidades compradas; la recepción puede seguir pendiente
	CostUnit    float64 // costo unitario en moneda (no cents)
	CostTotal   float64 // CostUnit * Delta — pre-computado para evitar N+1 en FE
	Reason      *string
	OccurredAt  time.Time
}

// RealizedProductProfit — desglose de la ganancia realizada de productos
// en un rango. Revenue y COGS en pesos; el caso de uso calcula
// realized = Revenue − COGS y arma el KPI. ItemsTotal/ItemsWithCost es la
// cobertura: cuántos productos con actividad tienen costo completo en todas
// sus líneas (los demás suman a Revenue, pero no exponen un margen engañoso).
type RealizedProductProfit struct {
	Revenue       float64
	COGS          float64
	ItemsTotal    int
	ItemsWithCost int
}

// ProductSalesTotals — $ y unidades de productos vendidos en un rango. Van
// juntos porque el KPI los pinta juntos ("$3,420 · 87 uds"): Amount sale de
// payments (concept='product'); Units de sale_items vía sales.
type ProductSalesTotals struct {
	Amount float64
	Units  int
}

// DailyIncome — one bar of the dashboard chart (UC-033).
//
// JSON tags son explícitos porque el wire layer (reports_controller) emite
// esta struct DIRECTAMENTE como `income_30d` / `income_by_day` sin DTO
// intermedio. Sin tags, Go marshalaba como "Date"/"Total" y la gráfica
// del FE (que lee data.date / data.total) renderizaba vacío.
type DailyIncome struct {
	Date  time.Time `json:"date"`
	Total float64   `json:"total"`
}

// MemberExpiringRow — vencen ≤7 días.
type MemberExpiringRow struct {
	MemberID       uuid.UUID
	FullName       string
	Phone          string
	ExpiryDate     time.Time
	DaysLeft       int
	MembershipType string
}

// MemberExpiredRow — vencidos hace ≤60 días, sin marca lost, sin contacto reciente.
type MemberExpiredRow struct {
	MemberID             uuid.UUID
	FullName             string
	Phone                string
	ExpiryDate           time.Time
	DaysOverdue          int
	LastContactAttemptAt *time.Time
	MembershipType       string
	ContactAttemptsCount int
}

// MemberInactiveRow — status='active' AND no checkin >21 días.
type MemberInactiveRow struct {
	MemberID      uuid.UUID
	FullName      string
	Phone         string
	LastCheckinAt *time.Time
	DaysAbsent    int
}

// ProductLowStockRow — stock <= stock_minimum.
type ProductLowStockRow struct {
	ProductID    uuid.UUID
	Name         string
	Stock        int
	StockMinimum int
}

// PendingBalanceRow — deuda viva TOTAL del socio: SUM(balance_pending) de
// todos sus pagos vivos (mismo agregado que billing.SumPendingByMember).
// PaymentDate es la fecha del pago con deuda más viejo — el "debe desde"
// que muestran dashboard y atención requerida.
type PendingBalanceRow struct {
	MemberID       uuid.UUID
	FullName       string
	Phone          string
	BalancePending float64
	PaymentDate    time.Time
}

// MemberBirthdayRow — cumpleañeros del día (mes+día match, año ignorado).
type MemberBirthdayRow struct {
	MemberID  uuid.UUID
	FullName  string
	Phone     string
	Birthdate time.Time
}

// MemberExportRow — flat row for the listado de socios export.
type MemberExportRow struct {
	Folio      string
	FullName   string
	Phone      string
	Email      *string
	Status     string
	PlanName   *string
	StartDate  *time.Time
	ExpiryDate *time.Time
	CreatedAt  time.Time
}

// PaymentExportRow — flat row for the pagos export.
type PaymentExportRow struct {
	Folio          string
	PaymentDate    time.Time
	MemberFullName *string
	Concept        string
	Method         string
	Amount         float64
	Discount       float64
	BalancePending float64
	OperatorEmail  *string
}

// RecentPaymentRow — one entry of the dashboard's "últimos cobros" widget.
// Member fields are nullable because product-only sales do not link to a
// member.
type RecentPaymentRow struct {
	ID          uuid.UUID
	MemberID    *uuid.UUID
	MemberName  *string
	Amount      float64
	Method      string
	Concept     string
	PaymentDate time.Time
	// SaleSummary — productos de la venta ("Agua 1L ×2 · Proteína") cuando
	// el pago es concept='product'. Título del widget para ventas walk-in
	// (sin socio), que antes se pintaban como "—".
	SaleSummary *string
}

// SaleExportRow — flat row for the ventas export.
type SaleExportRow struct {
	PaymentFolio string
	CreatedAt    time.Time
	MemberName   *string
	Subtotal     float64
	Discount     float64
	Total        float64
	Method       string
}
