//go:build server

// Package infraestructure holds the Postgres-backed Reader implementation for
// the reports application layer. Lives next to (not inside) the bounded
// contexts because the queries deliberately cross BCs — joining members,
// memberships, payments, products, and checkins in single-roundtrip rollups.
//
// We use raw SQL where a JOIN is involved (GORM's Model/Joins API gets
// awkward for the kind of "membership-current" + "last-checkin" patterns we
// need). Single-table aggregates use the Model/Select chain.
package infraestructure

import (
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/application/reports"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// PostgresReader implements reports.Reader against PG via GORM.
type PostgresReader struct{}

func NewPostgresReader() *PostgresReader { return &PostgresReader{} }

const dateFmt = "2006-01-02"

// ---------------------------------------------------------------------------
// Dashboard KPIs
// ---------------------------------------------------------------------------

func (r *PostgresReader) CountActiveMembers(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Raw(`
		SELECT COUNT(*) FROM members m
		JOIN memberships ms ON ms.member_id = m.id
		    AND ms.status = 'active' AND ms.deleted_at IS NULL
		WHERE m.gym_id = ?
		  AND m.status = 'active'
		  AND m.deleted_at IS NULL
		  AND ms.expiry_date >= ?`, gymID, today.Format(dateFmt)).Scan(&n).Error
	return int(n), err
}

func (r *PostgresReader) SumPaymentsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var total float64
	err := gormTx.Raw(`
		SELECT COALESCE(SUM(amount), 0) FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept <> 'refund'
		  AND payment_date >= ? AND payment_date <= ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&total).Error
	return total, err
}

func (r *PostgresReader) CountExpiringBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Raw(`
		SELECT COUNT(*) FROM memberships ms
		JOIN members m ON m.id = ms.member_id AND m.deleted_at IS NULL
		WHERE ms.gym_id = ? AND ms.deleted_at IS NULL
		  AND ms.status = 'active'
		  AND m.status = 'active'
		  AND ms.expiry_date >= ? AND ms.expiry_date <= ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&n).Error
	return int(n), err
}

func (r *PostgresReader) CountExpiredRecoverable(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, withinDays int) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	cutoff := today.AddDate(0, 0, -withinDays)
	var n int64
	err := gormTx.Raw(`
		SELECT COUNT(DISTINCT m.id) FROM members m
		JOIN memberships ms ON ms.member_id = m.id AND ms.deleted_at IS NULL
		WHERE m.gym_id = ? AND m.deleted_at IS NULL
		  AND m.status <> 'lost'
		  AND ms.expiry_date < ? AND ms.expiry_date >= ?`,
		gymID, today.Format(dateFmt), cutoff.Format(dateFmt)).Scan(&n).Error
	return int(n), err
}

func (r *PostgresReader) TodayCashByMethod(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (map[string]float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		Method string
		Total  float64
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT payment_method AS method, COALESCE(SUM(amount), 0) AS total
		FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept <> 'refund'
		  AND payment_date = ?
		GROUP BY payment_method`,
		gymID, today.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Method] = r.Total
	}
	return out, nil
}

func (r *PostgresReader) IncomeDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]reports.DailyIncome, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		Day   time.Time
		Total float64
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT payment_date AS day, COALESCE(SUM(amount), 0) AS total
		FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept <> 'refund'
		  AND payment_date >= ? AND payment_date <= ?
		GROUP BY payment_date
		ORDER BY payment_date`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.DailyIncome, len(rows))
	for i, x := range rows {
		out[i] = reports.DailyIncome{Date: x.Day, Total: x.Total}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Attention required
// ---------------------------------------------------------------------------

func (r *PostgresReader) ListExpiringSoon(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, days int) ([]reports.MemberExpiringRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	endDate := today.AddDate(0, 0, days)
	type row struct {
		MemberID       uuid.UUID
		FullName       string
		Phone          string
		ExpiryDate     time.Time
		MembershipType string
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT m.id AS member_id, m.full_name, m.phone, ms.expiry_date,
		       ms.type_name_snapshot AS membership_type
		FROM members m
		JOIN memberships ms ON ms.member_id = m.id AND ms.deleted_at IS NULL
		WHERE m.gym_id = ? AND m.deleted_at IS NULL
		  AND m.status = 'active'
		  AND ms.status = 'active'
		  AND ms.expiry_date >= ? AND ms.expiry_date <= ?
		ORDER BY ms.expiry_date ASC`,
		gymID, today.Format(dateFmt), endDate.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.MemberExpiringRow, len(rows))
	for i, x := range rows {
		out[i] = reports.MemberExpiringRow{
			MemberID:       x.MemberID,
			FullName:       x.FullName,
			Phone:          x.Phone,
			ExpiryDate:     x.ExpiryDate,
			DaysLeft:       daysBetween(today, x.ExpiryDate),
			MembershipType: x.MembershipType,
		}
	}
	return out, nil
}

func (r *PostgresReader) ListExpiredRecoverable(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, withinDays int, staleContactDays int) ([]reports.MemberExpiredRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	cutoff := today.AddDate(0, 0, -withinDays)
	staleAfter := today.AddDate(0, 0, -staleContactDays)
	type row struct {
		MemberID             uuid.UUID
		FullName             string
		Phone                string
		ExpiryDate           time.Time
		LastContactAttemptAt *time.Time
		MembershipType       string
		ContactAttemptsCount int
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT m.id AS member_id, m.full_name, m.phone,
		       ms.expiry_date,
		       m.last_contact_attempt_at,
		       ms.type_name_snapshot AS membership_type,
		       (SELECT COUNT(1) FROM contact_attempts ca
		        WHERE ca.member_id = m.id AND ca.deleted_at IS NULL) AS contact_attempts_count
		FROM members m
		JOIN memberships ms ON ms.member_id = m.id AND ms.deleted_at IS NULL
		WHERE m.gym_id = ? AND m.deleted_at IS NULL
		  AND m.status <> 'lost'
		  AND ms.expiry_date < ? AND ms.expiry_date >= ?
		  AND (m.last_contact_attempt_at IS NULL OR m.last_contact_attempt_at < ?)
		ORDER BY ms.expiry_date DESC`,
		gymID, today.Format(dateFmt), cutoff.Format(dateFmt), staleAfter).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.MemberExpiredRow, len(rows))
	for i, x := range rows {
		out[i] = reports.MemberExpiredRow{
			MemberID:             x.MemberID,
			FullName:             x.FullName,
			Phone:                x.Phone,
			ExpiryDate:           x.ExpiryDate,
			DaysOverdue:          -daysBetween(today, x.ExpiryDate),
			LastContactAttemptAt: x.LastContactAttemptAt,
			MembershipType:       x.MembershipType,
			ContactAttemptsCount: x.ContactAttemptsCount,
		}
	}
	return out, nil
}

func (r *PostgresReader) ListInactiveInvoluntary(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, daysWithoutCheckin int) ([]reports.MemberInactiveRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	cutoff := today.AddDate(0, 0, -daysWithoutCheckin)
	type row struct {
		MemberID      uuid.UUID
		FullName      string
		Phone         string
		LastCheckinAt *time.Time
	}
	var rows []row
	// Per-member latest checkin via correlated subquery. For a single gym
	// with O(1k) members this is fine; if it ever isn't, swap for a
	// LATERAL JOIN.
	if err := gormTx.Raw(`
		SELECT m.id AS member_id, m.full_name, m.phone,
		       (SELECT MAX(c.checkin_at) FROM checkins c
		        WHERE c.member_id = m.id AND c.deleted_at IS NULL
		          AND c.result LIKE 'allowed%') AS last_checkin_at
		FROM members m
		WHERE m.gym_id = ? AND m.deleted_at IS NULL
		  AND m.status = 'active'
		  AND ((SELECT MAX(c.checkin_at) FROM checkins c
		        WHERE c.member_id = m.id AND c.deleted_at IS NULL
		          AND c.result LIKE 'allowed%') IS NULL
		    OR (SELECT MAX(c.checkin_at) FROM checkins c
		        WHERE c.member_id = m.id AND c.deleted_at IS NULL
		          AND c.result LIKE 'allowed%') < ?)
		ORDER BY last_checkin_at NULLS FIRST`,
		gymID, cutoff).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.MemberInactiveRow, len(rows))
	for i, x := range rows {
		days := daysWithoutCheckin
		if x.LastCheckinAt != nil {
			diff := today.Sub(*x.LastCheckinAt).Hours() / 24
			if diff > 0 {
				days = int(diff)
			}
		}
		out[i] = reports.MemberInactiveRow{
			MemberID:      x.MemberID,
			FullName:      x.FullName,
			Phone:         x.Phone,
			LastCheckinAt: x.LastCheckinAt,
			DaysAbsent:    days,
		}
	}
	return out, nil
}

func (r *PostgresReader) ListLowStock(tx sharedDomain.Transaction, gymID uuid.UUID) ([]reports.ProductLowStockRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		ProductID    uuid.UUID
		Name         string
		Stock        int
		StockMinimum int
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT id AS product_id, name, stock, stock_minimum
		FROM products
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND active = TRUE
		  AND stock <= stock_minimum
		ORDER BY name ASC`, gymID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.ProductLowStockRow, len(rows))
	for i, x := range rows {
		out[i] = reports.ProductLowStockRow(x)
	}
	return out, nil
}

func (r *PostgresReader) ListPendingBalances(tx sharedDomain.Transaction, gymID uuid.UUID) ([]reports.PendingBalanceRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		MemberID       uuid.UUID
		FullName       string
		Phone          string
		BalancePending float64
		PaymentDate    time.Time
	}
	var rows []row
	// "Pending balance" = the most recent payment row for the member that
	// still carries balance_pending > 0 AND has not been settled by a
	// later 'balance_settlement' row. We approximate "open balance" by
	// taking the latest payment per member with balance_pending > 0
	// where no later settlement exists.
	if err := gormTx.Raw(`
		WITH latest AS (
		    SELECT DISTINCT ON (p.member_id) p.member_id, p.balance_pending, p.payment_date
		    FROM payments p
		    WHERE p.gym_id = ? AND p.deleted_at IS NULL
		      AND p.member_id IS NOT NULL
		      AND p.concept <> 'refund'
		    ORDER BY p.member_id, p.payment_date DESC, p.created_at DESC
		)
		SELECT l.member_id, m.full_name, m.phone, l.balance_pending, l.payment_date
		FROM latest l
		JOIN members m ON m.id = l.member_id AND m.deleted_at IS NULL
		WHERE l.balance_pending > 0
		ORDER BY l.balance_pending DESC`,
		gymID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.PendingBalanceRow, len(rows))
	for i, x := range rows {
		out[i] = reports.PendingBalanceRow(x)
	}
	return out, nil
}

func (r *PostgresReader) ListBirthdaysOn(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) ([]reports.MemberBirthdayRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		MemberID  uuid.UUID
		FullName  string
		Phone     string
		Birthdate time.Time
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT id AS member_id, full_name, phone, birthdate
		FROM members
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND status <> 'lost'
		  AND birthdate IS NOT NULL
		  AND EXTRACT(MONTH FROM birthdate) = ?
		  AND EXTRACT(DAY FROM birthdate) = ?
		ORDER BY full_name`,
		gymID, int(today.Month()), today.Day()).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.MemberBirthdayRow, len(rows))
	for i, x := range rows {
		out[i] = reports.MemberBirthdayRow(x)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Exports
// ---------------------------------------------------------------------------

func (r *PostgresReader) ListMembersForExport(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) ([]reports.MemberExportRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
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
	var rows []row
	if err := gormTx.Raw(`
		SELECT m.folio, m.full_name, m.phone, m.email, m.status,
		       ms.type_name_snapshot AS plan_name,
		       ms.start_date, ms.expiry_date,
		       m.created_at
		FROM members m
		LEFT JOIN memberships ms ON ms.member_id = m.id
		    AND ms.status = 'active' AND ms.deleted_at IS NULL
		WHERE m.gym_id = ? AND m.deleted_at IS NULL
		ORDER BY m.full_name`, gymID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.MemberExportRow, len(rows))
	for i, x := range rows {
		out[i] = reports.MemberExportRow(x)
	}
	return out, nil
}

func (r *PostgresReader) ListPaymentsForExport(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]reports.PaymentExportRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
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
	var rows []row
	if err := gormTx.Raw(`
		SELECT p.folio, p.payment_date, m.full_name AS member_full_name,
		       p.concept, p.payment_method AS method,
		       p.amount, p.discount_amount AS discount, p.balance_pending,
		       u.email AS operator_email
		FROM payments p
		LEFT JOIN members m ON m.id = p.member_id AND m.deleted_at IS NULL
		LEFT JOIN users u ON u.id = p.operator_id AND u.deleted_at IS NULL
		WHERE p.gym_id = ? AND p.deleted_at IS NULL
		  AND p.payment_date >= ? AND p.payment_date <= ?
		ORDER BY p.payment_date DESC, p.created_at DESC`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.PaymentExportRow, len(rows))
	for i, x := range rows {
		out[i] = reports.PaymentExportRow(x)
	}
	return out, nil
}

func (r *PostgresReader) ListSalesForExport(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]reports.SaleExportRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		PaymentFolio string
		CreatedAt    time.Time
		MemberName   *string
		Subtotal     float64
		Discount     float64
		Total        float64
		Method       string
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT p.folio AS payment_folio, s.created_at,
		       m.full_name AS member_name,
		       s.subtotal, s.discount, s.total,
		       p.payment_method AS method
		FROM sales s
		JOIN payments p ON p.id = s.payment_id AND p.deleted_at IS NULL
		LEFT JOIN members m ON m.id = s.member_id AND m.deleted_at IS NULL
		WHERE s.gym_id = ? AND s.deleted_at IS NULL
		  AND s.created_at >= ? AND s.created_at < ?
		ORDER BY s.created_at DESC`,
		gymID, from.UTC(), to.AddDate(0, 0, 1).UTC()).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.SaleExportRow, len(rows))
	for i, x := range rows {
		out[i] = reports.SaleExportRow(x)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Range report extras (UC-036)
// ---------------------------------------------------------------------------

func (r *PostgresReader) CountNewMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	// Calendar-day window: members whose registration falls in [from..to].
	// created_at is TIMESTAMPTZ; treat it as a date for filtering so the
	// bounds match the FE's date-only period selector.
	err := gormTx.Raw(`
		SELECT COUNT(*) FROM members
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND created_at::date >= ? AND created_at::date <= ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&n).Error
	return int(n), err
}

func (r *PostgresReader) CountCheckinsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Raw(`
		SELECT COUNT(*) FROM checkins
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND result LIKE 'allowed%'
		  AND checkin_at::date >= ? AND checkin_at::date <= ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&n).Error
	return int(n), err
}

func (r *PostgresReader) SumRefundsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var total float64
	// Refund rows are stored with negative amounts (concept='refund'); the
	// dashboard surfaces the absolute value so the operator sees "Devuelto:
	// $250" not "-$250".
	err := gormTx.Raw(`
		SELECT COALESCE(SUM(ABS(amount)), 0) FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept = 'refund'
		  AND payment_date >= ? AND payment_date <= ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&total).Error
	return total, err
}

func (r *PostgresReader) IncomeByMethodBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (map[string]float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		Method string
		Total  float64
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT payment_method AS method, COALESCE(SUM(amount), 0) AS total
		FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept <> 'refund'
		  AND payment_date >= ? AND payment_date <= ?
		GROUP BY payment_method`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Method] = r.Total
	}
	return out, nil
}

func (r *PostgresReader) TopMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]reports.TopMemberRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 {
		limit = 5
	}
	type row struct {
		MemberID      uuid.UUID
		FullName      string
		TotalPaid     float64
		PaymentsCount int
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT p.member_id, m.full_name,
		       COALESCE(SUM(p.amount), 0) AS total_paid,
		       COUNT(*) AS payments_count
		FROM payments p
		JOIN members m ON m.id = p.member_id AND m.deleted_at IS NULL
		WHERE p.gym_id = ? AND p.deleted_at IS NULL
		  AND p.concept <> 'refund'
		  AND p.member_id IS NOT NULL
		  AND p.payment_date >= ? AND p.payment_date <= ?
		GROUP BY p.member_id, m.full_name
		ORDER BY total_paid DESC
		LIMIT ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt), limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.TopMemberRow, len(rows))
	for i, x := range rows {
		out[i] = reports.TopMemberRow(x)
	}
	return out, nil
}

func (r *PostgresReader) CheckinsDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]reports.DailyCount, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		Day   time.Time
		Count int
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT checkin_at::date AS day, COUNT(*) AS count
		FROM checkins
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND result LIKE 'allowed%'
		  AND checkin_at::date >= ? AND checkin_at::date <= ?
		GROUP BY checkin_at::date
		ORDER BY checkin_at::date`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.DailyCount, len(rows))
	for i, x := range rows {
		out[i] = reports.DailyCount{Date: x.Day, Count: x.Count}
	}
	return out, nil
}

func (r *PostgresReader) ListRecentPayments(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]reports.RecentPaymentRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 {
		limit = 10
	}
	type row struct {
		ID          uuid.UUID
		MemberID    *uuid.UUID
		MemberName  *string
		Amount      float64
		Method      string
		Concept     string
		PaymentDate time.Time
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT p.id, p.member_id,
		       m.full_name AS member_name,
		       p.amount, p.payment_method AS method, p.concept,
		       p.payment_date
		FROM payments p
		LEFT JOIN members m ON m.id = p.member_id AND m.deleted_at IS NULL
		WHERE p.gym_id = ? AND p.deleted_at IS NULL
		  AND p.concept <> 'refund'
		ORDER BY p.payment_date DESC, p.created_at DESC
		LIMIT ?`, gymID, limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.RecentPaymentRow, len(rows))
	for i, x := range rows {
		out[i] = reports.RecentPaymentRow(x)
	}
	return out, nil
}

// SumInventoryCostBetween — análogo a SumRefundsBetween pero sobre
// stock_movements. cost (numeric(12,2)) es costo unitario; el total real
// del egreso por fila es cost*delta. Filtramos NULL para no contar
// restocks sin costo capturado.
func (r *PostgresReader) SumInventoryCostBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var total float64
	// Comparamos contra created_at (timestamp con TZ) directamente —
	// from/to llegan ya en UTC desde el use case.
	err := gormTx.Raw(`
		SELECT COALESCE(SUM(cost * delta), 0) FROM stock_movements
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND movement_type = 'restock'
		  AND cost IS NOT NULL
		  AND created_at >= ? AND created_at < ?`,
		gymID, from, to.AddDate(0, 0, 1)).Scan(&total).Error
	return total, err
}

// ListInventoryCostsBetween — JOIN a products para incluir el nombre y
// evitar el N+1 desde el FE. ORDER DESC porque la tabla del FE muestra
// el último egreso arriba.
func (r *PostgresReader) ListInventoryCostsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]reports.InventoryCostRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 {
		limit = 200
	}
	type row struct {
		MovementID  uuid.UUID
		ProductID   uuid.UUID
		ProductName string
		Delta       int
		Cost        float64
		Reason      *string
		CreatedAt   time.Time
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT sm.id AS movement_id, sm.product_id, p.name AS product_name,
		       sm.delta, sm.cost, sm.reason, sm.created_at
		FROM stock_movements sm
		JOIN products p ON p.id = sm.product_id
		WHERE sm.gym_id = ? AND sm.deleted_at IS NULL
		  AND sm.movement_type = 'restock'
		  AND sm.cost IS NOT NULL
		  AND sm.created_at >= ? AND sm.created_at < ?
		ORDER BY sm.created_at DESC
		LIMIT ?`, gymID, from, to.AddDate(0, 0, 1), limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.InventoryCostRow, len(rows))
	for i, x := range rows {
		out[i] = reports.InventoryCostRow{
			MovementID:  x.MovementID,
			ProductID:   x.ProductID,
			ProductName: x.ProductName,
			Delta:       x.Delta,
			CostUnit:    x.Cost,
			CostTotal:   x.Cost * float64(x.Delta),
			Reason:      x.Reason,
			OccurredAt:  x.CreatedAt.UTC(),
		}
	}
	return out, nil
}

// SumExpensesBetween — totaliza gastos generales (BC expenses) en el
// rango. Filtra por expense_date (el día que pasó el gasto según el
// dueño), no por created_at, para que "egresos del mes" refleje cuándo
// ocurrió el gasto y no cuándo se capturó.
func (r *PostgresReader) SumExpensesBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var total float64
	err := gormTx.Raw(`
		SELECT COALESCE(SUM(amount), 0) FROM expenses
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND expense_date >= ? AND expense_date <= ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&total).Error
	return total, err
}

// ListExpensesBetween — gastos del rango ordenados por expense_date DESC.
// Limit configurable; default 200 cuando llega 0.
func (r *PostgresReader) ListExpensesBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]reports.ExpenseRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 {
		limit = 200
	}
	type row struct {
		ID            uuid.UUID
		ExpenseDate   time.Time
		Amount        float64
		Category      string
		Description   *string
		PaymentMethod string
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT id, expense_date, amount, category, description, payment_method
		FROM expenses
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND expense_date >= ? AND expense_date <= ?
		ORDER BY expense_date DESC, created_at DESC
		LIMIT ?`, gymID, from.Format(dateFmt), to.Format(dateFmt), limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.ExpenseRow, len(rows))
	for i, x := range rows {
		out[i] = reports.ExpenseRow{
			ID:            x.ID,
			ExpenseDate:   x.ExpenseDate.UTC(),
			Amount:        x.Amount,
			Category:      x.Category,
			Description:   x.Description,
			PaymentMethod: x.PaymentMethod,
		}
	}
	return out, nil
}

// ExpensesDailySeries — total egresado por día sumando expenses + restocks
// con costo. Hacemos las dos queries por separado y mergeamos en memoria
// (los gyms tienen pocos días por ventana — O(días) keys), evita un UNION
// complejo y mantiene el filtro por fecha homogéneo entre las dos fuentes.
func (r *PostgresReader) ExpensesDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]reports.DailyAmount, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		Day   time.Time
		Total float64
	}
	bucket := map[string]float64{}

	var expRows []row
	if err := gormTx.Raw(`
		SELECT expense_date AS day, COALESCE(SUM(amount), 0) AS total
		FROM expenses
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND expense_date >= ? AND expense_date <= ?
		GROUP BY expense_date`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&expRows).Error; err != nil {
		return nil, err
	}
	for _, x := range expRows {
		bucket[x.Day.Format(dateFmt)] += x.Total
	}

	var invRows []row
	if err := gormTx.Raw(`
		SELECT created_at::date AS day, COALESCE(SUM(cost * delta), 0) AS total
		FROM stock_movements
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND movement_type = 'restock'
		  AND cost IS NOT NULL
		  AND created_at >= ? AND created_at < ?
		GROUP BY created_at::date`,
		gymID, from, to.AddDate(0, 0, 1)).Scan(&invRows).Error; err != nil {
		return nil, err
	}
	for _, x := range invRows {
		bucket[x.Day.Format(dateFmt)] += x.Total
	}

	out := make([]reports.DailyAmount, 0, len(bucket))
	for day, total := range bucket {
		t, _ := time.Parse(dateFmt, day)
		out = append(out, reports.DailyAmount{Date: t, Total: total})
	}
	// Sort ascending by date — the FE chart expects chronological order.
	sortDailyAmount(out)
	return out, nil
}

// ExpensesByCategoryBetween — total por categoría dentro del rango. No
// incluye compras de mercancía (eso vive en stock_movements y se reporta
// como "Compras de inventario" aparte).
func (r *PostgresReader) ExpensesByCategoryBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (map[string]float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		Category string
		Total    float64
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT category, COALESCE(SUM(amount), 0) AS total
		FROM expenses
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND expense_date >= ? AND expense_date <= ?
		GROUP BY category`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Category] = r.Total
	}
	return out, nil
}

// TopProductsBetween — ranking por revenue (= price * quantity de
// sale_items). JOIN con payments para filtrar por payment_date — la misma
// ventana que usa el resto de las queries de UC-036.
func (r *PostgresReader) TopProductsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]reports.TopProductRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 {
		limit = 5
	}
	type row struct {
		ProductID   uuid.UUID
		ProductName string
		Quantity    int
		Revenue     float64
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT si.product_id,
		       MIN(si.product_name_snapshot) AS product_name,
		       COALESCE(SUM(si.quantity), 0) AS quantity,
		       COALESCE(SUM(si.line_total), 0) AS revenue
		FROM sale_items si
		JOIN sales s ON s.id = si.sale_id AND s.deleted_at IS NULL
		JOIN payments p ON p.id = s.payment_id AND p.deleted_at IS NULL
		WHERE si.gym_id = ? AND si.deleted_at IS NULL
		  AND p.concept <> 'refund'
		  AND p.payment_date >= ? AND p.payment_date <= ?
		GROUP BY si.product_id
		ORDER BY revenue DESC
		LIMIT ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt), limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.TopProductRow, len(rows))
	for i, x := range rows {
		out[i] = reports.TopProductRow{
			ProductID:   x.ProductID,
			ProductName: x.ProductName,
			Quantity:    x.Quantity,
			Revenue:     x.Revenue,
		}
	}
	return out, nil
}

// CountCriticalStock — snapshot del catálogo. Out = stock = 0; Low = stock
// >0 pero <= stock_minimum. Solo productos activos no borrados.
func (r *PostgresReader) CountCriticalStock(tx sharedDomain.Transaction, gymID uuid.UUID) (reports.CriticalStockCounts, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var out struct {
		OutCount int
		LowCount int
	}
	err := gormTx.Raw(`
		SELECT
		    COUNT(*) FILTER (WHERE stock <= 0) AS out_count,
		    COUNT(*) FILTER (WHERE stock > 0 AND stock <= stock_minimum) AS low_count
		FROM products
		WHERE gym_id = ? AND deleted_at IS NULL AND active = TRUE`,
		gymID).Scan(&out).Error
	return reports.CriticalStockCounts{OutCount: out.OutCount, LowCount: out.LowCount}, err
}

// ---------------------------------------------------------------------------
// Gender reports
// ---------------------------------------------------------------------------

// GenderComposition — un solo round-trip que cuenta activos por bucket. NULL
// y los valores fuera del enum (no debería pasar por el CHECK constraint, pero
// nos defendemos) caen a no_especificado. Activos = mismo criterio que
// CountActiveMembers para mantener coherencia con el KPI de la página principal.
func (r *PostgresReader) GenderComposition(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (reports.GenderCompositionRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row struct {
		Hombre         int
		Mujer          int
		NoEspecificado int
		Total          int
	}
	err := gormTx.Raw(`
		SELECT
		    COUNT(*) FILTER (WHERE m.gender = 'hombre')                                AS hombre,
		    COUNT(*) FILTER (WHERE m.gender = 'mujer')                                 AS mujer,
		    COUNT(*) FILTER (WHERE m.gender IS NULL OR m.gender = 'no_especificado')   AS no_especificado,
		    COUNT(*)                                                                   AS total
		FROM members m
		JOIN memberships ms ON ms.member_id = m.id
		    AND ms.status = 'active' AND ms.deleted_at IS NULL
		WHERE m.gym_id = ?
		  AND m.status = 'active'
		  AND m.deleted_at IS NULL
		  AND ms.expiry_date >= ?`,
		gymID, today.Format(dateFmt)).Scan(&row).Error
	return reports.GenderCompositionRow{
		Hombre: row.Hombre, Mujer: row.Mujer,
		NoEspecificado: row.NoEspecificado, Total: row.Total,
	}, err
}

// AttendanceByGenderHour — heatmap de check-ins exitosos × hora × género.
// "Exitoso" = result LIKE 'allowed_%' (allowed_active / expiring_soon /
// override). Filtramos denied_* para que el reporte refleje uso real del gym,
// no intentos fallidos. La ventana corre [now - daysBack, now] sobre
// checkin_at (que es TIMESTAMPTZ — la conversión a hora local del gym
// queda para una mejora futura; hoy usamos UTC para simplificar).
func (r *PostgresReader) AttendanceByGenderHour(tx sharedDomain.Transaction, gymID uuid.UUID, daysBack int, now time.Time) ([]reports.AttendanceByGenderHourRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	cutoff := now.Add(-time.Duration(daysBack) * 24 * time.Hour)
	var rows []hourBucketRow
	err := gormTx.Raw(`
		SELECT
		    EXTRACT(HOUR FROM c.checkin_at)::int                                     AS hour,
		    COUNT(*) FILTER (WHERE m.gender = 'hombre')                              AS hombre,
		    COUNT(*) FILTER (WHERE m.gender = 'mujer')                               AS mujer,
		    COUNT(*) FILTER (WHERE m.gender IS NULL OR m.gender = 'no_especificado') AS no_especificado
		FROM checkins c
		JOIN members m ON m.id = c.member_id AND m.deleted_at IS NULL
		WHERE c.gym_id = ?
		  AND c.deleted_at IS NULL
		  AND c.result LIKE 'allowed_%'
		  AND c.checkin_at >= ? AND c.checkin_at <= ?
		GROUP BY EXTRACT(HOUR FROM c.checkin_at)`,
		gymID, cutoff, now).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return fillHourlyGenderGrid(rows), nil
}

// daysBetween returns floor((to - from) in days). Negative when `to` is before
// `from`. Both arguments are interpreted at day granularity.
func daysBetween(from, to time.Time) int {
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a).Hours() / 24)
}

// sortDailyAmount sorts a slice in-place ascending by Date. Tiny helper kept
// in this file to avoid pulling in sort.Slice from callers — daily series
// are small (≤365 entries even for "1 year").
func sortDailyAmount(s []reports.DailyAmount) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].Date.After(s[j].Date); j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
