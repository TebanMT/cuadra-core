//go:build sidecar

// SQLite-backed Reader for the reports application layer. Mirrors the
// Postgres implementation so the offline-first sidecar can serve the
// dashboard, the persecución list, the range report, and the exports
// directly from the local SQLite without round-tripping the cloud
// (CUADRA-SPEC §offline-first).
//
// Schema differences vs Postgres that drove these queries:
//   - UUIDs are TEXT (`gym_id.String()` at the binding edge).
//   - created_at/updated_at/checkin_at/last_contact_attempt_at are stored as
//     INTEGER unix milliseconds — date casting goes through
//     `date(col/1000, 'unixepoch')`.
//   - payment_date / start_date / expiry_date / birthdate are TEXT in
//     `YYYY-MM-DD`, so direct lexicographic comparisons work.
//   - Money is stored in cents (INTEGER); we divide by 100 at the edge to
//     match the float64 contract on Reader.
//   - `EXTRACT(MONTH/DAY FROM birthdate)` becomes `strftime('%m'/'%d', …)`.
//   - Postgres `DISTINCT ON` is rewritten as a `ROW_NUMBER() OVER (...)`
//     window function (SQLite ≥3.25 has these).
package infraestructure

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/application/reports"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// SQLiteReader implements reports.Reader against the local sidecar SQLite.
type SQLiteReader struct{}

func NewSQLiteReader() *SQLiteReader { return &SQLiteReader{} }

const sqliteDateFmt = "2006-01-02"

// ---------------------------------------------------------------------------
// Dashboard KPIs
// ---------------------------------------------------------------------------

func (r *SQLiteReader) CountActiveMembers(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n, `
		SELECT COUNT(*) FROM members m
		JOIN memberships ms ON ms.member_id = m.id
		    AND ms.status = 'active' AND ms.deleted_at IS NULL
		WHERE m.gym_id = ?
		  AND m.status = 'active'
		  AND m.deleted_at IS NULL
		  AND ms.expiry_date >= ?`,
		gymID.String(), today.Format(sqliteDateFmt))
	return n, err
}

func (r *SQLiteReader) SumPaymentsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var cents sql.NullInt64
	err := stx.Get(context.Background(), &cents, `
		SELECT COALESCE(SUM(amount), 0) FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept <> 'refund'
		  AND payment_date >= ? AND payment_date <= ?`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt))
	return float64(cents.Int64) / 100, err
}

func (r *SQLiteReader) CountExpiringBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n, `
		SELECT COUNT(*) FROM memberships ms
		JOIN members m ON m.id = ms.member_id AND m.deleted_at IS NULL
		WHERE ms.gym_id = ? AND ms.deleted_at IS NULL
		  AND ms.status = 'active'
		  AND m.status = 'active'
		  AND ms.expiry_date >= ? AND ms.expiry_date <= ?`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt))
	return n, err
}

func (r *SQLiteReader) CountExpiredRecoverable(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, withinDays int) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	cutoff := today.AddDate(0, 0, -withinDays)
	var n int
	err := stx.Get(context.Background(), &n, `
		SELECT COUNT(DISTINCT m.id) FROM members m
		JOIN memberships ms ON ms.member_id = m.id AND ms.deleted_at IS NULL
		WHERE m.gym_id = ? AND m.deleted_at IS NULL
		  AND m.status <> 'lost'
		  AND ms.expiry_date < ? AND ms.expiry_date >= ?`,
		gymID.String(), today.Format(sqliteDateFmt), cutoff.Format(sqliteDateFmt))
	return n, err
}

func (r *SQLiteReader) TodayCashByMethod(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (map[string]float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		Method string `db:"method"`
		Total  int64  `db:"total"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT payment_method AS method, COALESCE(SUM(amount), 0) AS total
		FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept <> 'refund'
		  AND payment_date = ?
		GROUP BY payment_method`,
		gymID.String(), today.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, x := range rows {
		out[x.Method] = float64(x.Total) / 100
	}
	return out, nil
}

func (r *SQLiteReader) IncomeDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]reports.DailyIncome, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		Day   string `db:"day"`
		Total int64  `db:"total"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT payment_date AS day, COALESCE(SUM(amount), 0) AS total
		FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept <> 'refund'
		  AND payment_date >= ? AND payment_date <= ?
		GROUP BY payment_date
		ORDER BY payment_date`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	out := make([]reports.DailyIncome, 0, len(rows))
	for _, x := range rows {
		t, err := time.Parse(sqliteDateFmt, x.Day)
		if err != nil {
			return nil, err
		}
		out = append(out, reports.DailyIncome{Date: t, Total: float64(x.Total) / 100})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Attention required
// ---------------------------------------------------------------------------

func (r *SQLiteReader) ListExpiringSoon(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, days int) ([]reports.MemberExpiringRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	endDate := today.AddDate(0, 0, days)
	type row struct {
		MemberID       string `db:"member_id"`
		FullName       string `db:"full_name"`
		Phone          string `db:"phone"`
		ExpiryDate     string `db:"expiry_date"`
		MembershipType string `db:"membership_type"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT m.id AS member_id, m.full_name, m.phone, ms.expiry_date,
		       ms.type_name_snapshot AS membership_type
		FROM members m
		JOIN memberships ms ON ms.member_id = m.id AND ms.deleted_at IS NULL
		WHERE m.gym_id = ? AND m.deleted_at IS NULL
		  AND m.status = 'active'
		  AND ms.status = 'active'
		  AND ms.expiry_date >= ? AND ms.expiry_date <= ?
		ORDER BY ms.expiry_date ASC`,
		gymID.String(), today.Format(sqliteDateFmt), endDate.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	out := make([]reports.MemberExpiringRow, 0, len(rows))
	for _, x := range rows {
		expiry, err := time.Parse(sqliteDateFmt, x.ExpiryDate)
		if err != nil {
			return nil, err
		}
		mid, _ := uuid.Parse(x.MemberID)
		out = append(out, reports.MemberExpiringRow{
			MemberID:       mid,
			FullName:       x.FullName,
			Phone:          x.Phone,
			ExpiryDate:     expiry,
			DaysLeft:       daysBetweenSqlite(today, expiry),
			MembershipType: x.MembershipType,
		})
	}
	return out, nil
}

func (r *SQLiteReader) ListExpiredRecoverable(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, withinDays int, staleContactDays int) ([]reports.MemberExpiredRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	cutoff := today.AddDate(0, 0, -withinDays)
	staleAfter := today.AddDate(0, 0, -staleContactDays).UnixMilli()
	type row struct {
		MemberID             string        `db:"member_id"`
		FullName             string        `db:"full_name"`
		Phone                string        `db:"phone"`
		ExpiryDate           string        `db:"expiry_date"`
		LastContactAttemptAt sql.NullInt64 `db:"last_contact_attempt_at"`
		MembershipType       string        `db:"membership_type"`
		ContactAttemptsCount int           `db:"contact_attempts_count"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
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
		gymID.String(), today.Format(sqliteDateFmt), cutoff.Format(sqliteDateFmt), staleAfter); err != nil {
		return nil, err
	}
	out := make([]reports.MemberExpiredRow, 0, len(rows))
	for _, x := range rows {
		expiry, err := time.Parse(sqliteDateFmt, x.ExpiryDate)
		if err != nil {
			return nil, err
		}
		mid, _ := uuid.Parse(x.MemberID)
		entry := reports.MemberExpiredRow{
			MemberID:             mid,
			FullName:             x.FullName,
			Phone:                x.Phone,
			ExpiryDate:           expiry,
			DaysOverdue:          -daysBetweenSqlite(today, expiry),
			MembershipType:       x.MembershipType,
			ContactAttemptsCount: x.ContactAttemptsCount,
		}
		if x.LastContactAttemptAt.Valid {
			t := time.UnixMilli(x.LastContactAttemptAt.Int64).UTC()
			entry.LastContactAttemptAt = &t
		}
		out = append(out, entry)
	}
	return out, nil
}

func (r *SQLiteReader) ListInactiveInvoluntary(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, daysWithoutCheckin int) ([]reports.MemberInactiveRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	cutoffMs := today.AddDate(0, 0, -daysWithoutCheckin).UnixMilli()
	type row struct {
		MemberID      string        `db:"member_id"`
		FullName      string        `db:"full_name"`
		Phone         string        `db:"phone"`
		LastCheckinAt sql.NullInt64 `db:"last_checkin_at"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
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
		gymID.String(), cutoffMs); err != nil {
		return nil, err
	}
	out := make([]reports.MemberInactiveRow, 0, len(rows))
	for _, x := range rows {
		mid, _ := uuid.Parse(x.MemberID)
		entry := reports.MemberInactiveRow{
			MemberID: mid,
			FullName: x.FullName,
			Phone:    x.Phone,
		}
		days := daysWithoutCheckin
		if x.LastCheckinAt.Valid {
			t := time.UnixMilli(x.LastCheckinAt.Int64).UTC()
			entry.LastCheckinAt = &t
			diff := today.Sub(t).Hours() / 24
			if diff > 0 {
				days = int(diff)
			}
		}
		entry.DaysAbsent = days
		out = append(out, entry)
	}
	return out, nil
}

func (r *SQLiteReader) ListLowStock(tx sharedDomain.Transaction, gymID uuid.UUID) ([]reports.ProductLowStockRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		ProductID    string `db:"product_id"`
		Name         string `db:"name"`
		Stock        int    `db:"stock"`
		StockMinimum int    `db:"stock_minimum"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT id AS product_id, name, stock, stock_minimum
		FROM products
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND active = 1
		  AND stock <= stock_minimum
		ORDER BY name ASC`, gymID.String()); err != nil {
		return nil, err
	}
	out := make([]reports.ProductLowStockRow, 0, len(rows))
	for _, x := range rows {
		pid, _ := uuid.Parse(x.ProductID)
		out = append(out, reports.ProductLowStockRow{
			ProductID:    pid,
			Name:         x.Name,
			Stock:        x.Stock,
			StockMinimum: x.StockMinimum,
		})
	}
	return out, nil
}

func (r *SQLiteReader) ListPendingBalances(tx sharedDomain.Transaction, gymID uuid.UUID) ([]reports.PendingBalanceRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		MemberID       string `db:"member_id"`
		FullName       string `db:"full_name"`
		Phone          string `db:"phone"`
		BalancePending int64  `db:"balance_pending"`
		PaymentDate    string `db:"payment_date"`
	}
	var rows []row
	// Equivalent of Postgres' DISTINCT ON: pick the latest payment per member
	// (by payment_date DESC, then created_at DESC) then keep only the ones
	// whose latest still has balance_pending > 0.
	if err := stx.Select(context.Background(), &rows, `
		WITH ranked AS (
		    SELECT
		        p.member_id,
		        p.balance_pending,
		        p.payment_date,
		        ROW_NUMBER() OVER (
		            PARTITION BY p.member_id
		            ORDER BY p.payment_date DESC, p.created_at DESC
		        ) AS rn
		    FROM payments p
		    WHERE p.gym_id = ? AND p.deleted_at IS NULL
		      AND p.member_id IS NOT NULL
		      AND p.concept <> 'refund'
		)
		SELECT r.member_id, m.full_name, m.phone,
		       r.balance_pending, r.payment_date
		FROM ranked r
		JOIN members m ON m.id = r.member_id AND m.deleted_at IS NULL
		WHERE r.rn = 1 AND r.balance_pending > 0
		ORDER BY r.balance_pending DESC`,
		gymID.String()); err != nil {
		return nil, err
	}
	out := make([]reports.PendingBalanceRow, 0, len(rows))
	for _, x := range rows {
		mid, _ := uuid.Parse(x.MemberID)
		paymentDate, err := time.Parse(sqliteDateFmt, x.PaymentDate)
		if err != nil {
			return nil, err
		}
		out = append(out, reports.PendingBalanceRow{
			MemberID:       mid,
			FullName:       x.FullName,
			Phone:          x.Phone,
			BalancePending: float64(x.BalancePending) / 100,
			PaymentDate:    paymentDate,
		})
	}
	return out, nil
}

func (r *SQLiteReader) ListBirthdaysOn(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) ([]reports.MemberBirthdayRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		MemberID  string `db:"member_id"`
		FullName  string `db:"full_name"`
		Phone     string `db:"phone"`
		Birthdate string `db:"birthdate"`
	}
	var rows []row
	month := fmt.Sprintf("%02d", int(today.Month()))
	day := fmt.Sprintf("%02d", today.Day())
	if err := stx.Select(context.Background(), &rows, `
		SELECT id AS member_id, full_name, phone, birthdate
		FROM members
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND status <> 'lost'
		  AND birthdate IS NOT NULL
		  AND strftime('%m', birthdate) = ?
		  AND strftime('%d', birthdate) = ?
		ORDER BY full_name`,
		gymID.String(), month, day); err != nil {
		return nil, err
	}
	out := make([]reports.MemberBirthdayRow, 0, len(rows))
	for _, x := range rows {
		mid, _ := uuid.Parse(x.MemberID)
		bday, err := time.Parse(sqliteDateFmt, x.Birthdate)
		if err != nil {
			return nil, err
		}
		out = append(out, reports.MemberBirthdayRow{
			MemberID:  mid,
			FullName:  x.FullName,
			Phone:     x.Phone,
			Birthdate: bday,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Exports
// ---------------------------------------------------------------------------

func (r *SQLiteReader) ListMembersForExport(tx sharedDomain.Transaction, gymID uuid.UUID, _ time.Time) ([]reports.MemberExportRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		Folio      string         `db:"folio"`
		FullName   string         `db:"full_name"`
		Phone      string         `db:"phone"`
		Email      sql.NullString `db:"email"`
		Status     string         `db:"status"`
		PlanName   sql.NullString `db:"plan_name"`
		StartDate  sql.NullString `db:"start_date"`
		ExpiryDate sql.NullString `db:"expiry_date"`
		CreatedAt  int64          `db:"created_at"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT m.folio, m.full_name, m.phone, m.email, m.status,
		       ms.type_name_snapshot AS plan_name,
		       ms.start_date, ms.expiry_date,
		       m.created_at
		FROM members m
		LEFT JOIN memberships ms ON ms.member_id = m.id
		    AND ms.status = 'active' AND ms.deleted_at IS NULL
		WHERE m.gym_id = ? AND m.deleted_at IS NULL
		ORDER BY m.full_name`, gymID.String()); err != nil {
		return nil, err
	}
	out := make([]reports.MemberExportRow, 0, len(rows))
	for _, x := range rows {
		entry := reports.MemberExportRow{
			Folio:     x.Folio,
			FullName:  x.FullName,
			Phone:     x.Phone,
			Status:    x.Status,
			CreatedAt: time.UnixMilli(x.CreatedAt).UTC(),
		}
		if x.Email.Valid {
			v := x.Email.String
			entry.Email = &v
		}
		if x.PlanName.Valid {
			v := x.PlanName.String
			entry.PlanName = &v
		}
		if x.StartDate.Valid {
			t, err := time.Parse(sqliteDateFmt, x.StartDate.String)
			if err == nil {
				entry.StartDate = &t
			}
		}
		if x.ExpiryDate.Valid {
			t, err := time.Parse(sqliteDateFmt, x.ExpiryDate.String)
			if err == nil {
				entry.ExpiryDate = &t
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

func (r *SQLiteReader) ListPaymentsForExport(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]reports.PaymentExportRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		Folio          string         `db:"folio"`
		PaymentDate    string         `db:"payment_date"`
		MemberFullName sql.NullString `db:"member_full_name"`
		Concept        string         `db:"concept"`
		Method         string         `db:"method"`
		Amount         int64          `db:"amount"`
		Discount       int64          `db:"discount"`
		BalancePending int64          `db:"balance_pending"`
		OperatorEmail  sql.NullString `db:"operator_email"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
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
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	out := make([]reports.PaymentExportRow, 0, len(rows))
	for _, x := range rows {
		date, err := time.Parse(sqliteDateFmt, x.PaymentDate)
		if err != nil {
			return nil, err
		}
		entry := reports.PaymentExportRow{
			Folio:          x.Folio,
			PaymentDate:    date,
			Concept:        x.Concept,
			Method:         x.Method,
			Amount:         float64(x.Amount) / 100,
			Discount:       float64(x.Discount) / 100,
			BalancePending: float64(x.BalancePending) / 100,
		}
		if x.MemberFullName.Valid {
			v := x.MemberFullName.String
			entry.MemberFullName = &v
		}
		if x.OperatorEmail.Valid {
			v := x.OperatorEmail.String
			entry.OperatorEmail = &v
		}
		out = append(out, entry)
	}
	return out, nil
}

func (r *SQLiteReader) ListSalesForExport(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]reports.SaleExportRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		PaymentFolio string         `db:"payment_folio"`
		CreatedAt    int64          `db:"created_at"`
		MemberName   sql.NullString `db:"member_name"`
		Subtotal     int64          `db:"subtotal"`
		Discount     int64          `db:"discount"`
		Total        int64          `db:"total"`
		Method       string         `db:"method"`
	}
	var rows []row
	// `sales.created_at` is unix-ms in SQLite, so we compare against the
	// `[from 00:00 UTC .. to+1 00:00 UTC)` half-open window — same shape as
	// the Postgres implementation.
	fromMs := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	toMs := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1).UnixMilli()
	if err := stx.Select(context.Background(), &rows, `
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
		gymID.String(), fromMs, toMs); err != nil {
		return nil, err
	}
	out := make([]reports.SaleExportRow, 0, len(rows))
	for _, x := range rows {
		entry := reports.SaleExportRow{
			PaymentFolio: x.PaymentFolio,
			CreatedAt:    time.UnixMilli(x.CreatedAt).UTC(),
			Subtotal:     float64(x.Subtotal) / 100,
			Discount:     float64(x.Discount) / 100,
			Total:        float64(x.Total) / 100,
			Method:       x.Method,
		}
		if x.MemberName.Valid {
			v := x.MemberName.String
			entry.MemberName = &v
		}
		out = append(out, entry)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Range report extras (UC-036)
// ---------------------------------------------------------------------------

func (r *SQLiteReader) CountNewMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	fromMs := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	toMs := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1).UnixMilli()
	var n int
	err := stx.Get(context.Background(), &n, `
		SELECT COUNT(*) FROM members
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND created_at >= ? AND created_at < ?`,
		gymID.String(), fromMs, toMs)
	return n, err
}

func (r *SQLiteReader) CountCheckinsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	fromMs := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	toMs := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1).UnixMilli()
	var n int
	err := stx.Get(context.Background(), &n, `
		SELECT COUNT(*) FROM checkins
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND result LIKE 'allowed%'
		  AND checkin_at >= ? AND checkin_at < ?`,
		gymID.String(), fromMs, toMs)
	return n, err
}

func (r *SQLiteReader) SumRefundsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var cents sql.NullInt64
	err := stx.Get(context.Background(), &cents, `
		SELECT COALESCE(SUM(ABS(amount)), 0) FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept = 'refund'
		  AND payment_date >= ? AND payment_date <= ?`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt))
	return float64(cents.Int64) / 100, err
}

func (r *SQLiteReader) IncomeByMethodBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (map[string]float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		Method string `db:"method"`
		Total  int64  `db:"total"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT payment_method AS method, COALESCE(SUM(amount), 0) AS total
		FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept <> 'refund'
		  AND payment_date >= ? AND payment_date <= ?
		GROUP BY payment_method`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, x := range rows {
		out[x.Method] = float64(x.Total) / 100
	}
	return out, nil
}

func (r *SQLiteReader) TopMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]reports.TopMemberRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 {
		limit = 5
	}
	type row struct {
		MemberID      string `db:"member_id"`
		FullName      string `db:"full_name"`
		TotalPaid     int64  `db:"total_paid"`
		PaymentsCount int    `db:"payments_count"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
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
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt), limit); err != nil {
		return nil, err
	}
	out := make([]reports.TopMemberRow, 0, len(rows))
	for _, x := range rows {
		mid, _ := uuid.Parse(x.MemberID)
		out = append(out, reports.TopMemberRow{
			MemberID:      mid,
			FullName:      x.FullName,
			TotalPaid:     float64(x.TotalPaid) / 100,
			PaymentsCount: x.PaymentsCount,
		})
	}
	return out, nil
}

func (r *SQLiteReader) CheckinsDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]reports.DailyCount, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	fromMs := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	toMs := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1).UnixMilli()
	type row struct {
		Day   string `db:"day"`
		Count int    `db:"count"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT date(checkin_at/1000, 'unixepoch') AS day, COUNT(*) AS count
		FROM checkins
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND result LIKE 'allowed%'
		  AND checkin_at >= ? AND checkin_at < ?
		GROUP BY day
		ORDER BY day`,
		gymID.String(), fromMs, toMs); err != nil {
		return nil, err
	}
	out := make([]reports.DailyCount, 0, len(rows))
	for _, x := range rows {
		t, err := time.Parse(sqliteDateFmt, x.Day)
		if err != nil {
			return nil, err
		}
		out = append(out, reports.DailyCount{Date: t, Count: x.Count})
	}
	return out, nil
}

func (r *SQLiteReader) ListRecentPayments(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]reports.RecentPaymentRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 {
		limit = 10
	}
	type row struct {
		ID          string         `db:"id"`
		MemberID    sql.NullString `db:"member_id"`
		MemberName  sql.NullString `db:"member_name"`
		Amount      int64          `db:"amount"`
		Method      string         `db:"method"`
		Concept     string         `db:"concept"`
		PaymentDate string         `db:"payment_date"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT p.id, p.member_id,
		       m.full_name AS member_name,
		       p.amount, p.payment_method AS method, p.concept,
		       p.payment_date
		FROM payments p
		LEFT JOIN members m ON m.id = p.member_id AND m.deleted_at IS NULL
		WHERE p.gym_id = ? AND p.deleted_at IS NULL
		  AND p.concept <> 'refund'
		ORDER BY p.payment_date DESC, p.created_at DESC
		LIMIT ?`, gymID.String(), limit); err != nil {
		return nil, err
	}
	out := make([]reports.RecentPaymentRow, 0, len(rows))
	for _, x := range rows {
		id, _ := uuid.Parse(x.ID)
		date, err := time.Parse(sqliteDateFmt, x.PaymentDate)
		if err != nil {
			return nil, err
		}
		entry := reports.RecentPaymentRow{
			ID:          id,
			Amount:      float64(x.Amount) / 100,
			Method:      x.Method,
			Concept:     x.Concept,
			PaymentDate: date,
		}
		if x.MemberID.Valid {
			mid, _ := uuid.Parse(x.MemberID.String)
			entry.MemberID = &mid
		}
		if x.MemberName.Valid {
			v := x.MemberName.String
			entry.MemberName = &v
		}
		out = append(out, entry)
	}
	return out, nil
}

// daysBetweenSqlite returns floor((to - from) in days) treating both at day
// granularity. Matches the Postgres impl's `daysBetween`.
func daysBetweenSqlite(from, to time.Time) int {
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a).Hours() / 24)
}
