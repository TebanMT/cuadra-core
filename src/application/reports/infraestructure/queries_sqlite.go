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
package infraestructure

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/application/reports"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// SQLiteReader implements reports.Reader against the local sidecar SQLite.
type SQLiteReader struct{}

func NewSQLiteReader() *SQLiteReader { return &SQLiteReader{} }

const sqliteDateFmt = "2006-01-02"

// dayBoundsMs traduce un rango de días LOCALES del gym al rango de epoch-ms
// [inicio, fin) que le corresponde. Las columnas de instante en SQLite
// (created_at, checkin_at) son unix-ms absolutos, así que acotarlas con
// medianoche UTC — como se hacía antes — metía en el día siguiente todo lo
// ocurrido después de las 6 PM en CDMX.
func dayBoundsMs(tzName string, from, to time.Time) (int64, int64) {
	start, end := tz.DayBounds(tzName, from, to)
	return start.UnixMilli(), end.UnixMilli()
}

// localDayExpr arma el truncado a día LOCAL sobre una columna unix-ms.
// SQLite no trae base de zonas horarias, así que desplazamos el epoch por
// el offset del gym antes de truncar; el offset se calcula en Go con la
// tzdata embebida. Ver tz.OffsetSeconds para la salvedad de DST.
func localDayExpr(column string) string {
	return fmt.Sprintf("date((%s/1000) + ?, 'unixepoch')", column)
}

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
	// Deuda viva TOTAL por socio: SUM(balance_pending) de todos sus pagos
	// vivos — mismo predicado que billing.SumPendingByMember (el total del
	// perfil del socio), para que esta lista y ese número nunca discrepen.
	// La versión anterior tomaba sólo el pago MÁS RECIENTE (rn=1): un pago
	// nuevo saldado escondía la deuda vieja, y deudas repartidas en varios
	// pagos mostraban sólo la última. balance_pending > 0 pre-GROUP basta
	// porque el dominio nunca produce balances negativos, y deja que
	// MIN(payment_date) sea la deuda abierta más vieja (el "debe desde"
	// del wire).
	if err := stx.Select(context.Background(), &rows, `
		SELECT p.member_id, m.full_name, m.phone,
		       SUM(p.balance_pending) AS balance_pending,
		       MIN(p.payment_date)    AS payment_date
		FROM payments p
		JOIN members m ON m.id = p.member_id AND m.deleted_at IS NULL
		WHERE p.gym_id = ? AND p.deleted_at IS NULL
		  AND p.member_id IS NOT NULL
		  AND p.balance_pending > 0
		GROUP BY p.member_id, m.full_name, m.phone
		ORDER BY SUM(p.balance_pending) DESC, m.full_name ASC`,
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

func (r *SQLiteReader) ListSalesForExport(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) ([]reports.SaleExportRow, error) {
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
	fromMs, toMs := dayBoundsMs(tzName, from, to)
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

func (r *SQLiteReader) CountNewMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	fromMs, toMs := dayBoundsMs(tzName, from, to)
	var n int
	err := stx.Get(context.Background(), &n, `
		SELECT COUNT(*) FROM members
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND created_at >= ? AND created_at < ?`,
		gymID.String(), fromMs, toMs)
	return n, err
}

func (r *SQLiteReader) CountCheckinsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	fromMs, toMs := dayBoundsMs(tzName, from, to)
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

// IncomeByMembershipTypeBetween — espejo del postgres: atribución por la
// membresía con start_date más reciente <= payment_date (TEXT YYYY-MM-DD,
// comparación lexicográfica). Cents → pesos al edge.
func (r *SQLiteReader) IncomeByMembershipTypeBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (map[string]float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		TypeName string `db:"type_name"`
		Total    int64  `db:"total"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT COALESCE((
		         SELECT ms.type_name_snapshot FROM memberships ms
		         WHERE ms.member_id = p.member_id AND ms.deleted_at IS NULL
		           AND ms.start_date <= p.payment_date
		         ORDER BY ms.start_date DESC LIMIT 1
		       ), 'Sin tipo') AS type_name,
		       COALESCE(SUM(p.amount), 0) AS total
		FROM payments p
		WHERE p.gym_id = ? AND p.deleted_at IS NULL
		  AND p.concept = 'membership'
		  AND p.payment_date >= ? AND p.payment_date <= ?
		GROUP BY 1`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, x := range rows {
		out[x.TypeName] = float64(x.Total) / 100
	}
	return out, nil
}

// ActiveMembersByType — mismo predicado que CountActiveMembers, agrupado.
func (r *SQLiteReader) ActiveMembersByType(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (map[string]int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		TypeName string `db:"type_name"`
		N        int    `db:"n"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT ms.type_name_snapshot AS type_name, COUNT(*) AS n
		FROM members m
		JOIN memberships ms ON ms.member_id = m.id
		    AND ms.status = 'active' AND ms.deleted_at IS NULL
		WHERE m.gym_id = ?
		  AND m.status = 'active'
		  AND m.deleted_at IS NULL
		  AND ms.expiry_date >= ?
		GROUP BY 1`,
		gymID.String(), today.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, x := range rows {
		out[x.TypeName] = x.N
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

func (r *SQLiteReader) CheckinsDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) ([]reports.DailyCount, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	fromMs, toMs := dayBoundsMs(tzName, from, to)
	type row struct {
		Day   string `db:"day"`
		Count int    `db:"count"`
	}
	var rows []row
	// Agrupar exige clasificar cada fila por su día local, no sólo acotar
	// el rango — de ahí el desplazamiento por offset (ver localDayExpr).
	offset := tz.OffsetSeconds(tzName, from)
	if err := stx.Select(context.Background(), &rows, `
		SELECT `+localDayExpr("checkin_at")+` AS day, COUNT(*) AS count
		FROM checkins
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND result LIKE 'allowed%'
		  AND checkin_at >= ? AND checkin_at < ?
		GROUP BY day
		ORDER BY day`,
		offset, gymID.String(), fromMs, toMs); err != nil {
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

// SumInventoryCostBetween — totaliza desembolsos por mercancía en el
// rango (movimientos 'restock' con costo capturado). En SQLite cost se
// guarda en cents (INTEGER) y delta es unidades — el total real es
// cost*delta. Filtramos NULL para no contar restocks sin costo (el
// operador puede no haberlo conocido al momento del registro).
func (r *SQLiteReader) SumInventoryCostBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	fromMs, toMs := dayBoundsMs(tzName, from, to)
	var cents sql.NullInt64
	// is_purchase filtra las capturas de inventario preexistente (alta de
	// catálogo): llevan costo para el margen pero NO son dinero que salió.
	err := stx.Get(context.Background(), &cents, `
		SELECT COALESCE(SUM(cost * delta), 0) FROM stock_movements
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND movement_type = 'restock'
		  AND cost IS NOT NULL
		  AND is_purchase = 1
		  AND created_at >= ? AND created_at < ?`,
		gymID.String(), fromMs, toMs)
	return float64(cents.Int64) / 100, err
}

// RealizedProductProfitBetween — espejo SQLite de la versión Postgres.
// unit_price_snapshot y cost están en centavos; el costo unitario promedio
// ponderado usa (2*SUM(cost*delta)+den)/(2*den) (half-up entero) = paridad
// con ROUND(SUM(cost*delta)/SUM(delta),2) de Postgres. Todo se calcula en
// centavos y se divide /100 al edge. Filtra
// por payment_date (TEXT YYYY-MM-DD, comparación lexicográfica) y excluye
// ventas reembolsadas.
func (r *SQLiteReader) RealizedProductProfitBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (reports.RealizedProductProfit, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row struct {
		RevenueCents  sql.NullInt64 `db:"revenue"`
		CogsCents     sql.NullInt64 `db:"cogs"`
		ItemsTotal    int           `db:"items_total"`
		ItemsWithCost int           `db:"items_with_cost"`
	}
	err := stx.Get(context.Background(), &row, `
		SELECT
		  COALESCE(SUM(si.unit_price_snapshot * si.quantity), 0) AS revenue,
		  COALESCE(SUM(CASE WHEN c.avg_unit_cost IS NOT NULL THEN si.quantity * c.avg_unit_cost ELSE 0 END), 0) AS cogs,
		  COUNT(*) AS items_total,
		  COALESCE(SUM(CASE WHEN c.avg_unit_cost IS NOT NULL THEN 1 ELSE 0 END), 0) AS items_with_cost
		FROM sale_items si
		JOIN sales s ON s.id = si.sale_id AND s.deleted_at IS NULL
		JOIN payments p ON p.id = s.payment_id AND p.deleted_at IS NULL
		LEFT JOIN (
		  SELECT product_id, (2*SUM(cost * delta) + SUM(delta)) / (2*SUM(delta)) AS avg_unit_cost
		  FROM stock_movements
		  WHERE gym_id = ? AND movement_type = 'restock' AND cost IS NOT NULL AND deleted_at IS NULL
		  GROUP BY product_id
		  HAVING SUM(delta) > 0
		) c ON c.product_id = si.product_id
		WHERE si.gym_id = ? AND si.deleted_at IS NULL
		  AND p.payment_date >= ? AND p.payment_date <= ?
		  AND NOT EXISTS (
		    SELECT 1 FROM payments rfd
		    WHERE rfd.parent_payment_id = p.id AND rfd.concept = 'refund' AND rfd.deleted_at IS NULL
		  )`,
		gymID.String(), gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt))
	if err != nil {
		return reports.RealizedProductProfit{}, err
	}
	return reports.RealizedProductProfit{
		Revenue:       float64(row.RevenueCents.Int64) / 100,
		COGS:          float64(row.CogsCents.Int64) / 100,
		ItemsTotal:    row.ItemsTotal,
		ItemsWithCost: row.ItemsWithCost,
	}, nil
}

// ListInventoryCostsBetween — JOINea product_name. ORDER BY created_at
// DESC para que el último egreso quede arriba en la tabla del FE.
func (r *SQLiteReader) ListInventoryCostsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time, limit int) ([]reports.InventoryCostRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 {
		limit = 200
	}
	fromMs, toMs := dayBoundsMs(tzName, from, to)
	type row struct {
		MovementID  string         `db:"movement_id"`
		ProductID   string         `db:"product_id"`
		ProductName string         `db:"product_name"`
		Delta       int            `db:"delta"`
		CostCents   int64          `db:"cost"`
		Reason      sql.NullString `db:"reason"`
		CreatedAt   int64          `db:"created_at"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT sm.id AS movement_id, sm.product_id, p.name AS product_name,
		       sm.delta, sm.cost, sm.reason, sm.created_at
		FROM stock_movements sm
		JOIN products p ON p.id = sm.product_id
		WHERE sm.gym_id = ? AND sm.deleted_at IS NULL
		  AND sm.movement_type = 'restock'
		  AND sm.cost IS NOT NULL
		  AND sm.is_purchase = 1
		  AND sm.created_at >= ? AND sm.created_at < ?
		ORDER BY sm.created_at DESC
		LIMIT ?`, gymID.String(), fromMs, toMs, limit); err != nil {
		return nil, err
	}
	out := make([]reports.InventoryCostRow, 0, len(rows))
	for _, x := range rows {
		mid, _ := uuid.Parse(x.MovementID)
		pid, _ := uuid.Parse(x.ProductID)
		costUnit := float64(x.CostCents) / 100
		entry := reports.InventoryCostRow{
			MovementID:  mid,
			ProductID:   pid,
			ProductName: x.ProductName,
			Delta:       x.Delta,
			CostUnit:    costUnit,
			CostTotal:   costUnit * float64(x.Delta),
			OccurredAt:  time.UnixMilli(x.CreatedAt).UTC(),
		}
		if x.Reason.Valid {
			s := x.Reason.String
			entry.Reason = &s
		}
		out = append(out, entry)
	}
	return out, nil
}

// SumExpensesBetween — totaliza gastos generales (BC expenses) en el
// rango. expense_date es TEXT YYYY-MM-DD (orden lexicográfico). amount
// está en cents → fromCents al edge.
func (r *SQLiteReader) SumExpensesBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var cents sql.NullInt64
	err := stx.Get(context.Background(), &cents, `
		SELECT COALESCE(SUM(amount), 0) FROM expenses
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND expense_date >= ? AND expense_date <= ?`,
		gymID.String(), from.Format("2006-01-02"), to.Format("2006-01-02"))
	return float64(cents.Int64) / 100, err
}

// ListExpensesBetween — lista expenses del rango. ORDER BY expense_date
// DESC para que el último gasto quede arriba. amount en cents → float
// al edge.
func (r *SQLiteReader) ListExpensesBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]reports.ExpenseRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 {
		limit = 200
	}
	type row struct {
		ID            string         `db:"id"`
		ExpenseDate   string         `db:"expense_date"`
		AmountCents   int64          `db:"amount"`
		Category      string         `db:"category"`
		Description   sql.NullString `db:"description"`
		PaymentMethod string         `db:"payment_method"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT id, expense_date, amount, category, description, payment_method
		FROM expenses
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND expense_date >= ? AND expense_date <= ?
		ORDER BY expense_date DESC, created_at DESC
		LIMIT ?`, gymID.String(), from.Format("2006-01-02"), to.Format("2006-01-02"), limit); err != nil {
		return nil, err
	}
	out := make([]reports.ExpenseRow, 0, len(rows))
	for _, x := range rows {
		id, _ := uuid.Parse(x.ID)
		date, _ := time.Parse("2006-01-02", x.ExpenseDate)
		entry := reports.ExpenseRow{
			ID:            id,
			ExpenseDate:   date,
			Amount:        float64(x.AmountCents) / 100,
			Category:      x.Category,
			PaymentMethod: x.PaymentMethod,
		}
		if x.Description.Valid {
			s := x.Description.String
			entry.Description = &s
		}
		out = append(out, entry)
	}
	return out, nil
}

// ExpensesDailySeries — egresos por día = expenses (expense_date) +
// stock_movements restock con costo (created_at) + devoluciones
// (payments concept='refund', payment_date, en absoluto). Suma en memoria
// por día porque cada fuente vive en una tabla con un tipo de fecha
// distinto y un UNION complicaría el binding. Las devoluciones van aquí
// para que la serie cuadre con Totals.Net (que también las resta).
func (r *SQLiteReader) ExpensesDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) ([]reports.DailyAmount, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	bucket := map[string]int64{}

	type expRow struct {
		Day   string `db:"day"`
		Total int64  `db:"total"`
	}
	var expRows []expRow
	if err := stx.Select(context.Background(), &expRows, `
		SELECT expense_date AS day, COALESCE(SUM(amount), 0) AS total
		FROM expenses
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND expense_date >= ? AND expense_date <= ?
		GROUP BY expense_date`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	for _, x := range expRows {
		bucket[x.Day] += x.Total
	}

	fromMs, toMs := dayBoundsMs(tzName, from, to)
	type invRow struct {
		Day   string `db:"day"`
		Total int64  `db:"total"`
	}
	var invRows []invRow
	// Única de las tres fuentes que va por instante: se agrupa por día
	// local para que caiga en la misma barra que el gasto o la devolución
	// del mismo día del gym.
	invOffset := tz.OffsetSeconds(tzName, from)
	if err := stx.Select(context.Background(), &invRows, `
		SELECT `+localDayExpr("created_at")+` AS day,
		       COALESCE(SUM(cost * delta), 0) AS total
		FROM stock_movements
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND movement_type = 'restock'
		  AND cost IS NOT NULL
		  AND is_purchase = 1
		  AND created_at >= ? AND created_at < ?
		GROUP BY day`,
		invOffset, gymID.String(), fromMs, toMs); err != nil {
		return nil, err
	}
	for _, x := range invRows {
		bucket[x.Day] += x.Total
	}

	type refRow struct {
		Day   string `db:"day"`
		Total int64  `db:"total"`
	}
	var refRows []refRow
	if err := stx.Select(context.Background(), &refRows, `
		SELECT payment_date AS day, COALESCE(SUM(ABS(amount)), 0) AS total
		FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept = 'refund'
		  AND payment_date >= ? AND payment_date <= ?
		GROUP BY payment_date`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	for _, x := range refRows {
		bucket[x.Day] += x.Total
	}

	out := make([]reports.DailyAmount, 0, len(bucket))
	for day, total := range bucket {
		t, err := time.Parse(sqliteDateFmt, day)
		if err != nil {
			continue
		}
		out = append(out, reports.DailyAmount{Date: t, Total: float64(total) / 100})
	}
	sortDailyAmountSqlite(out)
	return out, nil
}

// ExpensesByCategoryBetween — SUM(amount) por categoría dentro del rango.
// Sólo BC expenses (las compras de mercancía no son una categoría aquí).
func (r *SQLiteReader) ExpensesByCategoryBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (map[string]float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		Category string `db:"category"`
		Total    int64  `db:"total"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT category, COALESCE(SUM(amount), 0) AS total
		FROM expenses
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND expense_date >= ? AND expense_date <= ?
		GROUP BY category`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, x := range rows {
		out[x.Category] = float64(x.Total) / 100
	}
	return out, nil
}

// TopProductsBetween — ranking por revenue (= SUM(line_total) en cents).
// JOIN con payments para filtrar por payment_date — alinea la ventana con
// el resto del use case.
func (r *SQLiteReader) TopProductsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]reports.TopProductRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 {
		limit = 5
	}
	type row struct {
		ProductID   string `db:"product_id"`
		ProductName string `db:"product_name"`
		Quantity    int    `db:"quantity"`
		Revenue     int64  `db:"revenue"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
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
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt), limit); err != nil {
		return nil, err
	}
	out := make([]reports.TopProductRow, 0, len(rows))
	for _, x := range rows {
		pid, _ := uuid.Parse(x.ProductID)
		out = append(out, reports.TopProductRow{
			ProductID:   pid,
			ProductName: x.ProductName,
			Quantity:    x.Quantity,
			Revenue:     float64(x.Revenue) / 100,
		})
	}
	return out, nil
}

// SumProductSalesBetween — espejo del PG: $ por payments concept='product'
// (INTEGER cents → /100 al salir), unidades por sale_items de esos pagos.
func (r *SQLiteReader) SumProductSalesBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (reports.ProductSalesTotals, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var cents sql.NullInt64
	if err := stx.Get(context.Background(), &cents, `
		SELECT COALESCE(SUM(amount), 0) FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept = 'product'
		  AND payment_date >= ? AND payment_date <= ?`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt)); err != nil {
		return reports.ProductSalesTotals{}, err
	}
	var units sql.NullInt64
	if err := stx.Get(context.Background(), &units, `
		SELECT COALESCE(SUM(si.quantity), 0)
		FROM sale_items si
		JOIN sales s ON s.id = si.sale_id AND s.deleted_at IS NULL
		JOIN payments p ON p.id = s.payment_id AND p.deleted_at IS NULL
		WHERE si.gym_id = ? AND si.deleted_at IS NULL
		  AND p.concept = 'product'
		  AND p.payment_date >= ? AND p.payment_date <= ?`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt)); err != nil {
		return reports.ProductSalesTotals{}, err
	}
	return reports.ProductSalesTotals{
		Amount: float64(cents.Int64) / 100,
		Units:  int(units.Int64),
	}, nil
}

// CountCriticalStock — snapshot del catálogo. SQLite usa active=1 boolean
// como integer.
func (r *SQLiteReader) CountCriticalStock(tx sharedDomain.Transaction, gymID uuid.UUID) (reports.CriticalStockCounts, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var out struct {
		OutCount int `db:"out_count"`
		LowCount int `db:"low_count"`
	}
	err := stx.Get(context.Background(), &out, `
		SELECT
		    SUM(CASE WHEN stock <= 0 THEN 1 ELSE 0 END) AS out_count,
		    SUM(CASE WHEN stock > 0 AND stock <= stock_minimum THEN 1 ELSE 0 END) AS low_count
		FROM products
		WHERE gym_id = ? AND deleted_at IS NULL AND active = 1`,
		gymID.String())
	return reports.CriticalStockCounts{OutCount: out.OutCount, LowCount: out.LowCount}, err
}

// ---------------------------------------------------------------------------
// Gender reports
// ---------------------------------------------------------------------------

// GenderComposition — espeja la versión postgres. SQLite no tiene
// FILTER (WHERE …), así que usamos SUM(CASE WHEN …) que produce el
// mismo resultado. Activos = misma definición que CountActiveMembers
// (members.status='active' + membership activa con expiry_date vigente).
func (r *SQLiteReader) GenderComposition(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (reports.GenderCompositionRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row struct {
		Hombre         int `db:"hombre"`
		Mujer          int `db:"mujer"`
		NoEspecificado int `db:"no_especificado"`
		Total          int `db:"total"`
	}
	err := stx.Get(context.Background(), &row, `
		SELECT
		    SUM(CASE WHEN m.gender = 'hombre' THEN 1 ELSE 0 END)                                AS hombre,
		    SUM(CASE WHEN m.gender = 'mujer' THEN 1 ELSE 0 END)                                 AS mujer,
		    SUM(CASE WHEN m.gender IS NULL OR m.gender = 'no_especificado' THEN 1 ELSE 0 END)   AS no_especificado,
		    COUNT(*)                                                                            AS total
		FROM members m
		JOIN memberships ms ON ms.member_id = m.id
		    AND ms.status = 'active' AND ms.deleted_at IS NULL
		WHERE m.gym_id = ?
		  AND m.status = 'active'
		  AND m.deleted_at IS NULL
		  AND ms.expiry_date >= ?`,
		gymID.String(), today.Format(sqliteDateFmt))
	return reports.GenderCompositionRow{
		Hombre: row.Hombre, Mujer: row.Mujer,
		NoEspecificado: row.NoEspecificado, Total: row.Total,
	}, err
}

// AttendanceByGenderHour — checkin_at en SQLite es INTEGER unix milliseconds.
// La hora se clasifica en la zona LOCAL del gym desplazando el epoch por el
// offset (misma técnica que localDayExpr) — con la hora UTC el heatmap salía
// corrido 6 horas en CDMX. result LIKE 'allowed_%' filtra denied_*.
func (r *SQLiteReader) AttendanceByGenderHour(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, daysBack int, now time.Time) ([]reports.AttendanceByGenderHourRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	cutoffMs := now.Add(-time.Duration(daysBack) * 24 * time.Hour).UnixMilli()
	nowMs := now.UnixMilli()
	offset := tz.OffsetSeconds(tzName, now)
	type sqliteRow struct {
		HourStr        string `db:"hour"`
		Hombre         int    `db:"hombre"`
		Mujer          int    `db:"mujer"`
		NoEspecificado int    `db:"no_especificado"`
	}
	var rows []sqliteRow
	err := stx.Select(context.Background(), &rows, `
		SELECT
		    strftime('%H', (c.checkin_at/1000) + ?, 'unixepoch')                                 AS hour,
		    SUM(CASE WHEN m.gender = 'hombre' THEN 1 ELSE 0 END)                                 AS hombre,
		    SUM(CASE WHEN m.gender = 'mujer' THEN 1 ELSE 0 END)                                  AS mujer,
		    SUM(CASE WHEN m.gender IS NULL OR m.gender = 'no_especificado' THEN 1 ELSE 0 END)    AS no_especificado
		FROM checkins c
		JOIN members m ON m.id = c.member_id AND m.deleted_at IS NULL
		WHERE c.gym_id = ?
		  AND c.deleted_at IS NULL
		  AND c.result LIKE 'allowed_%'
		  AND c.checkin_at >= ? AND c.checkin_at <= ?
		GROUP BY 1`,
		offset, gymID.String(), cutoffMs, nowMs)
	if err != nil {
		return nil, err
	}
	// strftime('%H', …) devuelve "00".."23"; parseamos a int para
	// reusar el grid builder shared.
	converted := make([]hourBucketRow, 0, len(rows))
	for _, r := range rows {
		var h int
		// fmt.Sscanf maneja el zero-pad sin problema.
		if _, perr := fmt.Sscanf(r.HourStr, "%d", &h); perr != nil {
			continue
		}
		converted = append(converted, hourBucketRow{
			Hour: h, Hombre: r.Hombre, Mujer: r.Mujer, NoEspecificado: r.NoEspecificado,
		})
	}
	return fillHourlyGenderGrid(converted), nil
}

// sortDailyAmountSqlite — insertion sort por fecha ascendente. Series cortas
// (≤365 entradas), no vale la pena pull-in de sort.Slice acá.
func sortDailyAmountSqlite(s []reports.DailyAmount) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].Date.After(s[j].Date); j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// daysBetweenSqlite returns floor((to - from) in days) treating both at day
// granularity. Matches the Postgres impl's `daysBetween`.
func daysBetweenSqlite(from, to time.Time) int {
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a).Hours() / 24)
}
