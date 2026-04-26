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
}

// DailyIncome — one bar of the dashboard chart (UC-033).
type DailyIncome struct {
	Date  time.Time
	Total float64
}

// MemberExpiringRow — vencen ≤7 días.
type MemberExpiringRow struct {
	MemberID   uuid.UUID
	FullName   string
	Phone      string
	ExpiryDate time.Time
	DaysLeft   int
}

// MemberExpiredRow — vencidos hace ≤60 días, sin marca lost, sin contacto reciente.
type MemberExpiredRow struct {
	MemberID             uuid.UUID
	FullName             string
	Phone                string
	ExpiryDate           time.Time
	DaysOverdue          int
	LastContactAttemptAt *time.Time
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
