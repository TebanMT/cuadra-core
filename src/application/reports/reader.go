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

// Reader is the cross-context query surface for reports.
type Reader interface {
	// Dashboard KPIs (UC-033)
	CountActiveMembers(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (int, error)
	SumPaymentsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error)
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
	ListSalesForExport(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]SaleExportRow, error)

	// Range report extras (UC-036 — totals + breakdowns over an arbitrary
	// window; complement the dashboard KPIs).
	CountNewMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (int, error)
	CountCheckinsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (int, error)
	SumRefundsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error)
	IncomeByMethodBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (map[string]float64, error)
	TopMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]TopMemberRow, error)
	CheckinsDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]DailyCount, error)

	// ListRecentPayments returns the latest non-refund payments for a gym,
	// ordered by payment_date DESC then created_at DESC. Used by the
	// dashboard's "últimos cobros" widget.
	ListRecentPayments(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]RecentPaymentRow, error)
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

// PendingBalanceRow — última fila de payments con balance_pending > 0 por socio.
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
