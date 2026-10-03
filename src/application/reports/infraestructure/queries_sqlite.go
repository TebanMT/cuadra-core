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
	"math"
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

// FinancialReadMetadata mirrors PostgreSQL's financial watermark and adds
// the offline fact the cloud cannot know: whether this gym still has local
// financial mutations waiting in sync_queue.
func (r *SQLiteReader) FinancialReadMetadata(tx sharedDomain.Transaction, gymID uuid.UUID) (reports.FinancialReadMetadata, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var watermark sql.NullInt64
	err := stx.Get(context.Background(), &watermark, `
		SELECT MAX(updated_at) FROM (
		  SELECT updated_at FROM payments WHERE gym_id=?
		  UNION ALL SELECT updated_at FROM sales WHERE gym_id=?
		  UNION ALL SELECT updated_at FROM sale_items WHERE gym_id=?
		  UNION ALL SELECT updated_at FROM refunds WHERE gym_id=?
		  UNION ALL SELECT updated_at FROM expenses WHERE gym_id=?
		  UNION ALL SELECT updated_at FROM inventory_purchases WHERE gym_id=?
		  UNION ALL SELECT updated_at FROM stock_movements WHERE gym_id=?
		  UNION ALL SELECT updated_at FROM cash_close_events WHERE gym_id=?
		  UNION ALL SELECT updated_at FROM cash_movements WHERE gym_id=?
		  UNION ALL SELECT updated_at FROM cash_transfers WHERE gym_id=?
		  UNION ALL SELECT updated_at FROM sale_corrections WHERE gym_id=?
		  UNION ALL SELECT updated_at FROM payment_corrections WHERE gym_id=?
		) financial_events`, gymID.String(), gymID.String(), gymID.String(), gymID.String(), gymID.String(), gymID.String(),
		gymID.String(), gymID.String(), gymID.String(), gymID.String(), gymID.String(), gymID.String())
	if err != nil {
		return reports.FinancialReadMetadata{}, err
	}
	var pendingInt int
	err = stx.Get(context.Background(), &pendingInt, `SELECT EXISTS(
		SELECT 1 FROM sync_queue
		WHERE synced_at IS NULL
		  AND entity_type IN ('payments','sales','sale_items','refunds','expenses','inventory_purchases',
		    'stock_movements','cash_close_events','cash_movements','cash_transfers','sale_corrections','payment_corrections')
		  AND json_extract(payload,'$.gym_id')=?
	)`, gymID.String())
	if err != nil {
		return reports.FinancialReadMetadata{}, err
	}
	pending := pendingInt != 0
	out := reports.FinancialReadMetadata{SyncPending: &pending}
	if watermark.Valid {
		v := time.UnixMilli(watermark.Int64).UTC()
		out.DataWatermark = &v
	}
	return out, nil
}

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
		WHERE m.gym_id = ?
		  AND m.status = 'active'
		  AND m.deleted_at IS NULL
		  AND EXISTS (
		      SELECT 1 FROM memberships ms
		      WHERE ms.member_id = m.id AND ms.deleted_at IS NULL
		        AND ms.status IN ('active', 'replaced')
		        AND ms.start_date <= ? AND ms.expiry_date >= ?
		  )`,
		gymID.String(), today.Format(sqliteDateFmt), today.Format(sqliteDateFmt))
	return n, err
}

func (r *SQLiteReader) SumPaymentsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var cents sql.NullInt64
	err := stx.Get(context.Background(), &cents, `
		SELECT COALESCE(SUM(recognized_amount), 0) FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept <> 'refund'
		  AND payment_date >= ? AND payment_date <= ?`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt))
	return float64(cents.Int64) / 100, err
}

func (r *SQLiteReader) SumOtherIncomeBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var cents sql.NullInt64
	err := stx.Get(context.Background(), &cents, `
		SELECT COALESCE(SUM(recognized_amount), 0) FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept = 'other'
		  AND payment_date >= ? AND payment_date <= ?`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt))
	return float64(cents.Int64) / 100, err
}

func (r *SQLiteReader) SumCashClosedBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var cents sql.NullInt64
	err := stx.Get(context.Background(), &cents, `
		SELECT
		  COALESCE((SELECT SUM(p.amount) FROM payments p
		    WHERE p.gym_id = ? AND p.deleted_at IS NULL AND p.payment_method = 'cash' AND p.amount<>0 AND p.cash_destination<>'gym_fund'
		      AND p.payment_date >= ? AND p.payment_date <= ?), 0)
		  + COALESCE((SELECT SUM(CASE WHEN m.movement_type = 'cash_in' THEN m.amount ELSE -m.amount END)
		    FROM cash_movements m
		    WHERE m.gym_id = ? AND m.deleted_at IS NULL
		      AND m.movement_on >= ? AND m.movement_on <= ?), 0)`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt))
	return float64(cents.Int64) / 100, err
}

func (r *SQLiteReader) SumCashCountedBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (reports.CashCountSummary, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row struct {
		Counted                         int64          `db:"counted"`
		CountedCloses                   int            `db:"counted_closes"`
		TotalCloses                     int            `db:"total_closes"`
		Withdrawn                       int64          `db:"withdrawn"`
		HistoricalActivityDays          int            `db:"historical_activity_days"`
		HistoricalSessions              int            `db:"historical_sessions"`
		ActiveDays                      int            `db:"active_days"`
		MissingActiveDays               int            `db:"missing_active_days"`
		UncoveredActivityDays           int            `db:"uncovered_activity_days"`
		OpenSessions                    int            `db:"open_sessions"`
		ClosedUnverifiedSessions        int            `db:"closed_unverified_sessions"`
		ReconciledSessions              int            `db:"reconciled_sessions"`
		StaleSessions                   int            `db:"stale_sessions"`
		WithdrawnSessions               int            `db:"withdrawn_sessions"`
		UnknownOpeningSessions          int            `db:"unknown_opening_sessions"`
		AdjustedAfterWithdrawalSessions int            `db:"adjusted_after_withdrawal_sessions"`
		LegacyCashSourceUnverifiedCount int            `db:"legacy_cash_source_unverified_count"`
		TotalSessions                   int            `db:"total_sessions"`
		LatestSessionID                 sql.NullString `db:"latest_session_id"`
		LatestExpected                  sql.NullInt64  `db:"latest_expected"`
		LatestCounted                   sql.NullInt64  `db:"latest_counted"`
		LatestDifference                sql.NullInt64  `db:"latest_difference"`
		LatestCountedAt                 sql.NullInt64  `db:"latest_counted_at"`
		LatestStatus                    sql.NullString `db:"latest_status"`
		LatestNeedsRecount              int            `db:"latest_needs_recount"`
	}
	err := stx.Get(context.Background(), &row, `
		WITH physical(day,drawer_id,recorded_at,touched_at,amount) AS (
		  SELECT payment_date,COALESCE(cash_drawer_id,gym_id),created_at,created_at,amount
		  FROM payments
		  WHERE gym_id=? AND payment_method='cash' AND amount<>0 AND cash_destination<>'gym_fund' AND deleted_at IS NULL
		    AND payment_date BETWEEN ? AND ?
		  UNION ALL
		  SELECT movement_on,COALESCE(cash_drawer_id,gym_id),created_at,created_at,
		         CASE WHEN movement_type='cash_in' THEN amount ELSE -amount END
		  FROM cash_movements
		  WHERE gym_id=? AND deleted_at IS NULL AND movement_on BETWEEN ? AND ?
		), active_dates(day) AS (
		  SELECT DISTINCT day FROM physical
		), gym_sessions AS (
		  SELECT * FROM cash_close_events WHERE gym_id = ?
		), tracking AS (
		  -- Real openings confirm opening cash; migrated snapshots do not.
		  -- Use shared business dates, never a device's migration timestamp.
		  -- Tombstones retain the start so deletion cannot reset coverage.
		  SELECT MIN(COALESCE(operational_date,close_date)) AS started_on
		  FROM gym_sessions WHERE opening_cash_known=1
		), period_sessions AS (
		  SELECT * FROM gym_sessions WHERE deleted_at IS NULL
		    AND COALESCE(operational_date,close_date) >= ?
		    AND COALESCE(operational_date,close_date) <= ?
		), scoped_base AS (
		  SELECT * FROM period_sessions
		  WHERE COALESCE(operational_date,close_date)>=(SELECT started_on FROM tracking)
		), tracked_physical AS (
		  SELECT * FROM physical WHERE day>=(SELECT started_on FROM tracking)
		), current_activity AS (
		  SELECT s.id,COALESCE(SUM(p.amount),0) AS amount
		  FROM scoped_base s
		  LEFT JOIN physical p
		    ON p.day=COALESCE(s.operational_date,s.close_date)
		   AND p.drawer_id=COALESCE(s.drawer_id,s.gym_id)
		   AND p.recorded_at>=COALESCE(s.opened_at,s.created_at)
		   AND (s.withdrawn_at IS NULL OR p.recorded_at<=s.withdrawn_at)
		  GROUP BY s.id
		), uncovered_dates(day) AS (
		  SELECT DISTINCT p.day FROM tracked_physical p
		  WHERE (SELECT COUNT(*) FROM scoped_base s
		         WHERE COALESCE(s.operational_date,s.close_date)=p.day
		           AND COALESCE(s.drawer_id,s.gym_id)=p.drawer_id
		           AND p.recorded_at>=COALESCE(s.opened_at,s.created_at)
		           AND (COALESCE(s.status,'closed_unverified')<>'withdrawn' OR s.withdrawn_at IS NOT NULL)
		           AND (s.withdrawn_at IS NULL OR p.recorded_at<=s.withdrawn_at)) <> 1
		     OR EXISTS (SELECT 1 FROM scoped_base s
		         WHERE COALESCE(s.operational_date,s.close_date)=p.day
		           AND COALESCE(s.drawer_id,s.gym_id)=p.drawer_id
		           AND p.recorded_at>=COALESCE(s.opened_at,s.created_at)
		           AND s.closed_at IS NOT NULL AND p.touched_at>s.closed_at
		           AND (COALESCE(s.status,'closed_unverified')<>'withdrawn'
		             OR (s.withdrawn_at IS NOT NULL AND p.touched_at<=s.withdrawn_at)))
		  UNION
		  SELECT COALESCE(s.operational_date,s.close_date)
		  FROM scoped_base s JOIN current_activity ca ON ca.id=s.id
		  WHERE COALESCE(s.status,'closed_unverified')<>'open'
		    AND ca.amount<>COALESCE(s.activity_cash,0)
		), scoped AS (
		  SELECT s.*,
		    CASE WHEN s.withdrawn_at IS NOT NULL
		                   AND ca.amount<>COALESCE(s.activity_cash,0)
		         THEN 1 ELSE 0 END AS dynamic_post_withdrawal_adjustment,
		    CASE WHEN s.status='stale' THEN 1
		      WHEN COALESCE(s.status,'closed_unverified') IN ('open','withdrawn') THEN 0
		      WHEN ca.amount<>COALESCE(s.activity_cash,0) THEN 1
		      WHEN s.closed_at IS NOT NULL AND EXISTS (
		        SELECT 1 FROM physical p
		        WHERE p.day=COALESCE(s.operational_date,s.close_date)
		          AND p.drawer_id=COALESCE(s.drawer_id,s.gym_id)
		          AND p.recorded_at>=COALESCE(s.opened_at,s.created_at)
		          AND p.recorded_at>s.closed_at
		          AND (s.withdrawn_at IS NULL OR p.recorded_at<=s.withdrawn_at)
		      ) THEN 1 ELSE 0 END AS needs_recount
		  FROM scoped_base s JOIN current_activity ca ON ca.id=s.id
		), effective AS (
		  SELECT s.*,
		    CASE WHEN needs_recount=1 THEN 'stale'
		         ELSE COALESCE(status,'closed_unverified') END AS effective_status
		  FROM scoped s
		), latest AS (
		  SELECT * FROM effective
		  WHERE effective_status <> 'open'
		  ORDER BY COALESCE(operational_date,close_date) DESC,
		           COALESCE(reconciled_at,closed_at,updated_at,created_at) DESC,
		           COALESCE(sequence,1) DESC, id DESC
		  LIMIT 1
		)
		SELECT COALESCE((SELECT SUM(counted_cash) FROM effective
		                 WHERE effective_status IN ('reconciled','withdrawn')),0) AS counted,
		       (SELECT COUNT(counted_cash) FROM effective
		        WHERE effective_status IN ('reconciled','withdrawn')) AS counted_closes,
		       (SELECT COUNT(*) FROM effective WHERE effective_status <> 'open') AS total_closes,
		       COALESCE((SELECT SUM(withdrawn_cash) FROM effective),0) AS withdrawn,
		       (SELECT COUNT(*) FROM active_dates) AS active_days,
		       (SELECT COUNT(*) FROM active_dates a WHERE NOT EXISTS (
		         SELECT 1 FROM tracking t WHERE a.day>=t.started_on)) AS historical_activity_days,
		       (SELECT COUNT(*) FROM period_sessions s WHERE NOT EXISTS (
		         SELECT 1 FROM tracking t WHERE COALESCE(s.operational_date,s.close_date)>=t.started_on)) AS historical_sessions,
		       (SELECT COUNT(*) FROM active_dates a
		        WHERE a.day>=(SELECT started_on FROM tracking) AND NOT EXISTS (
		          SELECT 1 FROM scoped_base s WHERE COALESCE(s.operational_date,s.close_date)=a.day
		        )) AS missing_active_days,
		       (SELECT COUNT(*) FROM uncovered_dates) AS uncovered_activity_days,
		       (SELECT COUNT(*) FROM effective WHERE effective_status='open') AS open_sessions,
		       (SELECT COUNT(*) FROM effective WHERE effective_status='closed_unverified') AS closed_unverified_sessions,
		       (SELECT COUNT(*) FROM effective WHERE effective_status='reconciled') AS reconciled_sessions,
		       (SELECT COUNT(*) FROM effective WHERE effective_status='stale') AS stale_sessions,
		       (SELECT COUNT(*) FROM effective WHERE effective_status='withdrawn') AS withdrawn_sessions,
		       (SELECT COUNT(*) FROM effective WHERE COALESCE(opening_cash_known,0)=0) AS unknown_opening_sessions,
		       (SELECT COUNT(*) FROM effective
		         WHERE COALESCE(adjusted_after_withdrawal,0)=1
		            OR dynamic_post_withdrawal_adjustment=1) AS adjusted_after_withdrawal_sessions,
		       (SELECT COUNT(*) FROM expenses e
		         WHERE e.gym_id=? AND e.deleted_at IS NULL
		           AND e.paid_from IN ('cash_register','cash_drawer')
		           AND e.cash_movement_id IS NULL
		           AND e.expense_date BETWEEN ? AND ?) AS legacy_cash_source_unverified_count,
		       (SELECT COUNT(*) FROM effective) AS total_sessions,
		       l.id AS latest_session_id,
		       l.calculated_cash AS latest_expected,
		       l.counted_cash AS latest_counted,
		       CASE WHEN l.counted_cash IS NOT NULL
		                  AND l.effective_status IN ('reconciled','withdrawn')
		                  AND COALESCE(l.adjusted_after_withdrawal,0)=0
		                  AND l.dynamic_post_withdrawal_adjustment=0
		            THEN l.counted_cash-l.calculated_cash END AS latest_difference,
		       CASE WHEN l.counted_cash IS NOT NULL
		            THEN COALESCE(l.reconciled_at,l.closed_at,l.updated_at) END AS latest_counted_at,
		       l.effective_status AS latest_status,
		       CASE WHEN COALESCE(l.needs_recount,0)=1
		                   OR COALESCE(l.dynamic_post_withdrawal_adjustment,0)=1
		                   OR COALESCE(l.adjusted_after_withdrawal,0)=1
		            THEN 1 ELSE 0 END AS latest_needs_recount
		FROM (SELECT 1) anchor LEFT JOIN latest l ON 1=1`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt))
	out := reports.CashCountSummary{
		Counted:                float64(row.Counted) / 100,
		CountedCloses:          row.CountedCloses,
		TotalCloses:            row.TotalCloses,
		Withdrawn:              float64(row.Withdrawn) / 100,
		HistoricalActivityDays: row.HistoricalActivityDays, HistoricalSessions: row.HistoricalSessions,
		ActiveDays: row.ActiveDays, MissingActiveDays: row.MissingActiveDays,
		UncoveredActivityDays: row.UncoveredActivityDays, OpenSessions: row.OpenSessions,
		ClosedUnverifiedSessions: row.ClosedUnverifiedSessions,
		ReconciledSessions:       row.ReconciledSessions, StaleSessions: row.StaleSessions,
		WithdrawnSessions: row.WithdrawnSessions, UnknownOpeningSessions: row.UnknownOpeningSessions,
		AdjustedAfterWithdrawalSessions: row.AdjustedAfterWithdrawalSessions,
		LegacyCashSourceUnverifiedCount: row.LegacyCashSourceUnverifiedCount,
		TotalSessions:                   row.TotalSessions,
	}
	if row.LatestSessionID.Valid {
		id, parseErr := uuid.Parse(row.LatestSessionID.String)
		if parseErr != nil {
			return reports.CashCountSummary{}, parseErr
		}
		out.LatestSessionID = &id
	}
	if row.LatestExpected.Valid {
		v := float64(row.LatestExpected.Int64) / 100
		out.LatestExpected = &v
	}
	if row.LatestCounted.Valid {
		v := float64(row.LatestCounted.Int64) / 100
		out.LatestCounted = &v
	}
	if row.LatestDifference.Valid {
		v := float64(row.LatestDifference.Int64) / 100
		out.LatestDifference = &v
	}
	if row.LatestCountedAt.Valid {
		v := time.UnixMilli(row.LatestCountedAt.Int64).UTC()
		out.LatestCountedAt = &v
	}
	if row.LatestStatus.Valid {
		out.LatestStatus = row.LatestStatus.String
	}
	out.LatestNeedsRecount = row.LatestNeedsRecount != 0
	return out, err
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
		  AND ms.start_date <= ?
		  AND ms.expiry_date >= ? AND ms.expiry_date <= ?`,
		gymID.String(), from.Format(sqliteDateFmt), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt))
	return n, err
}

func (r *SQLiteReader) CountExpiredRecoverable(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, withinDays int) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	cutoff := today.AddDate(0, 0, -withinDays)
	var n int
	err := stx.Get(context.Background(), &n, `
		SELECT COUNT(*) FROM members m
		WHERE m.gym_id = ? AND m.deleted_at IS NULL
		  AND m.status <> 'lost'
		  AND EXISTS (
		      SELECT 1 FROM memberships expired
		      WHERE expired.member_id = m.id AND expired.deleted_at IS NULL
		        AND expired.expiry_date < ? AND expired.expiry_date >= ?
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM memberships covered
		      WHERE covered.member_id = m.id AND covered.deleted_at IS NULL
		        AND covered.status IN ('active', 'replaced')
		        AND covered.start_date <= ? AND covered.expiry_date >= ?
		  )`, gymID.String(), today.Format(sqliteDateFmt), cutoff.Format(sqliteDateFmt),
		today.Format(sqliteDateFmt), today.Format(sqliteDateFmt))
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
		SELECT payment_date AS day, COALESCE(SUM(recognized_amount), 0) AS total
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
		  AND ms.start_date <= ?
		  AND ms.expiry_date >= ? AND ms.expiry_date <= ?
		ORDER BY ms.expiry_date ASC`,
		gymID.String(), today.Format(sqliteDateFmt), today.Format(sqliteDateFmt), endDate.Format(sqliteDateFmt)); err != nil {
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
		JOIN memberships ms ON ms.id = (
		    SELECT expired.id FROM memberships expired
		    WHERE expired.member_id = m.id AND expired.deleted_at IS NULL
		      AND expired.expiry_date < ? AND expired.expiry_date >= ?
		    ORDER BY expired.expiry_date DESC, expired.created_at DESC
		    LIMIT 1
		)
		WHERE m.gym_id = ? AND m.deleted_at IS NULL
		  AND m.status <> 'lost'
		  AND (m.last_contact_attempt_at IS NULL OR m.last_contact_attempt_at < ?)
		  AND NOT EXISTS (
		      SELECT 1 FROM memberships covered
		      WHERE covered.member_id = m.id AND covered.deleted_at IS NULL
		        AND covered.status IN ('active', 'replaced')
		        AND covered.start_date <= ? AND covered.expiry_date >= ?
		  )
		ORDER BY ms.expiry_date DESC`,
		today.Format(sqliteDateFmt), cutoff.Format(sqliteDateFmt), gymID.String(), staleAfter,
		today.Format(sqliteDateFmt), today.Format(sqliteDateFmt)); err != nil {
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
		  AND EXISTS (
		      SELECT 1 FROM memberships covered
		      WHERE covered.member_id = m.id AND covered.deleted_at IS NULL
		        AND covered.status IN ('active', 'replaced')
		        AND covered.start_date <= ? AND covered.expiry_date >= ?
		  )
		  AND ((SELECT MAX(c.checkin_at) FROM checkins c
		        WHERE c.member_id = m.id AND c.deleted_at IS NULL
		          AND c.result LIKE 'allowed%') IS NULL
		    OR (SELECT MAX(c.checkin_at) FROM checkins c
		        WHERE c.member_id = m.id AND c.deleted_at IS NULL
		          AND c.result LIKE 'allowed%') < ?)
		ORDER BY last_checkin_at NULLS FIRST`,
		gymID.String(), today.Format(sqliteDateFmt), today.Format(sqliteDateFmt), cutoffMs); err != nil {
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
		SELECT COALESCE(SUM(amount), 0) FROM refunds
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND kind = 'revenue_refund'
		  AND refunded_on >= ? AND refunded_on <= ?`,
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
		SELECT payment_method AS method, COALESCE(SUM(recognized_amount), 0) AS total
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

// IncomeByMembershipTypeBetween — espejo del postgres: cobro inicial y sus
// abonos se atribuyen primero por membership_id exacto; la inferencia por
// socio y fecha queda como fallback legacy. Cents → pesos al edge.
func (r *SQLiteReader) IncomeByMembershipTypeBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (map[string]float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		TypeName string `db:"type_name"`
		Total    int64  `db:"total"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT COALESCE(
		         (SELECT exact_ms.type_name_snapshot FROM memberships exact_ms
		          WHERE exact_ms.gym_id = p.gym_id
		            AND exact_ms.id = COALESCE(p.membership_id, parent.membership_id)),
		         (SELECT legacy_ms.type_name_snapshot FROM memberships legacy_ms
		          WHERE legacy_ms.member_id = COALESCE(p.member_id, parent.member_id)
		            AND legacy_ms.deleted_at IS NULL
		            AND legacy_ms.start_date <= COALESCE(parent.payment_date, p.payment_date)
		          ORDER BY legacy_ms.start_date DESC LIMIT 1),
		         'Sin tipo') AS type_name,
		       COALESCE(SUM(p.recognized_amount), 0) AS total
		FROM payments p
		LEFT JOIN payments parent ON parent.id = p.parent_payment_id
		  AND parent.gym_id = p.gym_id AND parent.deleted_at IS NULL
		WHERE p.gym_id = ? AND p.deleted_at IS NULL
		  AND (p.concept = 'membership'
		    OR (p.concept = 'balance_settlement' AND parent.concept = 'membership'))
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
		JOIN memberships ms ON ms.id = (
		    SELECT covered.id
		    FROM memberships covered
		    WHERE covered.member_id = m.id AND covered.deleted_at IS NULL
		      AND covered.status IN ('active', 'replaced')
		      AND covered.start_date <= ? AND covered.expiry_date >= ?
		    ORDER BY covered.expiry_date DESC,
		             CASE WHEN covered.status = 'active' THEN 0 ELSE 1 END,
		             covered.created_at DESC
		    LIMIT 1
		)
		WHERE m.gym_id = ?
		  AND m.status = 'active'
		  AND m.deleted_at IS NULL
		GROUP BY 1`,
		today.Format(sqliteDateFmt), today.Format(sqliteDateFmt), gymID.String()); err != nil {
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
		       COALESCE(SUM(p.recognized_amount), 0) AS total_paid,
		       COUNT(*) AS payments_count
		FROM payments p
		JOIN members m ON m.id = p.member_id AND m.deleted_at IS NULL
		WHERE p.gym_id = ? AND p.deleted_at IS NULL
		  AND p.concept <> 'refund' AND p.amount > 0
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
		SaleSummary sql.NullString `db:"sale_summary"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT p.id, p.member_id,
		       m.full_name AS member_name,
		       p.amount, p.payment_method AS method, p.concept,
		       p.payment_date,
		       (SELECT group_concat(
		          si.product_name_snapshot ||
		            CASE WHEN si.quantity > 1 THEN ' ×' || si.quantity ELSE '' END,
		          ' · ')
		        FROM sales s
		        JOIN sale_items si ON si.sale_id = s.id AND si.deleted_at IS NULL
		        WHERE s.payment_id = p.id AND s.deleted_at IS NULL) AS sale_summary
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
		if x.SaleSummary.Valid {
			v := x.SaleSummary.String
			entry.SaleSummary = &v
		}
		out = append(out, entry)
	}
	return out, nil
}

// SumInventoryCostBetween suma compras explícitas efectivamente pagadas y,
// durante la compatibilidad histórica, restocks marcados como compra cuyo
// monto todavía puede reconstruirse exactamente (cantidad × costo unitario).
// Un restock sin costo sigue fuera del total y se declara en integridad.
func (r *SQLiteReader) SumInventoryCostBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	fromMs, toMs := dayBoundsMs(tzName, from, to)
	var cents sql.NullInt64
	err := stx.Get(context.Background(), &cents, `
		SELECT COALESCE(SUM(amount),0) FROM (
		  SELECT total_amount AS amount
		  FROM inventory_purchases
		  WHERE gym_id=? AND deleted_at IS NULL AND status='paid'
		    AND paid_on>=? AND paid_on<=?
		  UNION ALL
		  SELECT COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost) AS amount
		  FROM stock_movements sm
		  LEFT JOIN inventory_purchases ip
		    ON ip.stock_movement_id=sm.id AND ip.deleted_at IS NULL
		  WHERE sm.gym_id=? AND sm.deleted_at IS NULL
		    AND sm.movement_type='restock' AND sm.is_purchase=1
		    AND sm.created_at>=? AND sm.created_at<? AND sm.delta>0
		    AND (ip.id IS NULL OR ip.status='legacy_incomplete')
		    AND COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost,0)>0
		) purchases`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), fromMs, toMs)
	return float64(cents.Int64) / 100, err
}

func (r *SQLiteReader) CanonicalFinancialBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (reports.CanonicalFinancialSnapshot, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	fromMs, toMs := dayBoundsMs(tzName, from, to)
	var row struct {
		MembershipIncome                 int64 `db:"membership_income"`
		ProductIncome                    int64 `db:"product_income"`
		OtherIncome                      int64 `db:"other_income"`
		UnclassifiedIncome               int64 `db:"unclassified_income"`
		OperatingExpenses                int64 `db:"operating_expenses"`
		InventoryPurchases               int64 `db:"inventory_purchases"`
		Refunds                          int64 `db:"refunds"`
		UnclassifiedIncomeCount          int   `db:"unclassified_income_count"`
		UnclassifiedCashOutCount         int   `db:"unclassified_cash_out_count"`
		InvalidCashInClassificationCount int   `db:"invalid_cash_in_classification_count"`
		LegacyCashSourceUnverifiedCount  int   `db:"legacy_cash_source_unverified_count"`
		LegacyPurchaseCount              int   `db:"legacy_purchase_count"`
		LegacyRefundCount                int   `db:"legacy_refund_count"`
	}
	err := stx.Get(context.Background(), &row, `
		WITH classified AS (
			  SELECT p.recognized_amount AS amount,
		         CASE WHEN p.concept='balance_settlement' THEN parent.concept ELSE p.concept END AS bucket
		  FROM payments p
		  LEFT JOIN payments parent ON parent.id=p.parent_payment_id
		  WHERE p.gym_id=? AND p.deleted_at IS NULL AND p.concept<>'refund'
		    AND p.payment_date>=? AND p.payment_date<=?
		)
		SELECT
		  COALESCE(SUM(CASE WHEN bucket='membership' THEN amount ELSE 0 END),0) AS membership_income,
		  COALESCE(SUM(CASE WHEN bucket='product' THEN amount ELSE 0 END),0) AS product_income,
		  COALESCE(SUM(CASE WHEN bucket='other' THEN amount ELSE 0 END),0) AS other_income,
		  COALESCE(SUM(CASE WHEN bucket IS NULL OR bucket NOT IN ('membership','product','other') THEN amount ELSE 0 END),0) AS unclassified_income,
		  (SELECT COALESCE(SUM(e.amount),0) FROM expenses e
		    WHERE e.gym_id=? AND e.deleted_at IS NULL AND e.expense_date>=? AND e.expense_date<=?) AS operating_expenses,
		  (SELECT COALESCE(SUM(amount),0) FROM (
		    SELECT ip.total_amount AS amount FROM inventory_purchases ip
		    WHERE ip.gym_id=? AND ip.deleted_at IS NULL AND ip.status='paid'
		      AND ip.paid_on>=? AND ip.paid_on<=?
		    UNION ALL
		    SELECT COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost) AS amount
		    FROM stock_movements sm
		    LEFT JOIN inventory_purchases ip
		      ON ip.stock_movement_id=sm.id AND ip.deleted_at IS NULL
		    WHERE sm.gym_id=? AND sm.deleted_at IS NULL AND sm.movement_type='restock'
		      AND sm.is_purchase=1 AND sm.created_at>=? AND sm.created_at<? AND sm.delta>0
		      AND (ip.id IS NULL OR ip.status='legacy_incomplete')
		      AND COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost,0)>0
		  )) AS inventory_purchases,
			  (SELECT COALESCE(SUM(r.amount),0) FROM refunds r
			    WHERE r.gym_id=? AND r.deleted_at IS NULL AND r.kind='revenue_refund'
			      AND r.refunded_on>=? AND r.refunded_on<=?) AS refunds,
		  COALESCE(SUM(CASE WHEN bucket IS NULL OR bucket NOT IN ('membership','product','other') THEN 1 ELSE 0 END),0) AS unclassified_income_count,
		  (SELECT COUNT(*) FROM cash_movements cm
		    WHERE cm.gym_id=? AND cm.deleted_at IS NULL AND cm.movement_type='cash_out'
		      AND cm.classification_status='unclassified' AND cm.movement_on>=? AND cm.movement_on<=?) AS unclassified_cash_out_count,
		  (SELECT COUNT(*) FROM cash_movements cm
		    WHERE cm.gym_id=? AND cm.deleted_at IS NULL AND cm.movement_type='cash_in'
		      AND (cm.classification_status<>'non_operating' OR cm.expense_id IS NOT NULL)
		      AND cm.movement_on>=? AND cm.movement_on<=?) AS invalid_cash_in_classification_count,
		  (SELECT COUNT(*) FROM expenses e
		    WHERE e.gym_id=? AND e.deleted_at IS NULL
		      AND e.paid_from IN ('cash_register','cash_drawer')
		      AND e.cash_movement_id IS NULL
		      AND e.expense_date>=? AND e.expense_date<=?) AS legacy_cash_source_unverified_count,
		  (SELECT COUNT(*) FROM stock_movements sm
		    LEFT JOIN inventory_purchases ip ON ip.stock_movement_id=sm.id AND ip.deleted_at IS NULL
		    WHERE sm.gym_id=? AND sm.deleted_at IS NULL AND sm.movement_type='restock'
		      AND sm.is_purchase=1 AND sm.created_at>=? AND sm.created_at<?
		      AND (ip.id IS NULL OR ip.status='legacy_incomplete')
		      AND COALESCE(NULLIF(ip.total_amount,0),
		        CASE WHEN sm.delta>0 AND sm.cost>0 THEN sm.delta*sm.cost END,0)<=0) AS legacy_purchase_count,
		  (SELECT COUNT(*) FROM payments rp
		    LEFT JOIN refunds rr ON rr.refund_payment_id=rp.id AND rr.deleted_at IS NULL
		    WHERE rp.gym_id=? AND rp.deleted_at IS NULL AND rp.concept='refund'
		      AND rp.payment_date>=? AND rp.payment_date<=?
		      AND (rr.id IS NULL OR rr.legacy_incomplete=1)) AS legacy_refund_count
		FROM classified`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), fromMs, toMs,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), fromMs, toMs,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt))
	if err != nil {
		return reports.CanonicalFinancialSnapshot{}, err
	}
	return reports.CanonicalFinancialSnapshot{
		MembershipIncome:                 float64(row.MembershipIncome) / 100,
		ProductIncome:                    float64(row.ProductIncome) / 100,
		OtherIncome:                      float64(row.OtherIncome) / 100,
		UnclassifiedIncome:               float64(row.UnclassifiedIncome) / 100,
		OperatingExpenses:                float64(row.OperatingExpenses) / 100,
		InventoryPurchases:               float64(row.InventoryPurchases) / 100,
		Refunds:                          float64(row.Refunds) / 100,
		UnclassifiedIncomeCount:          row.UnclassifiedIncomeCount,
		UnclassifiedCashOutCount:         row.UnclassifiedCashOutCount,
		InvalidCashInClassificationCount: row.InvalidCashInClassificationCount,
		LegacyCashSourceUnverifiedCount:  row.LegacyCashSourceUnverifiedCount,
		LegacyPurchaseCount:              row.LegacyPurchaseCount,
		LegacyRefundCount:                row.LegacyRefundCount,
	}, nil
}

func (r *SQLiteReader) ProductProfitabilityBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]reports.ProductProfitabilityRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		ProductID    string  `db:"product_id"`
		ProductName  string  `db:"product_name"`
		Quantity     int     `db:"quantity"`
		RevenueCents float64 `db:"revenue"`
		CogsCents    float64 `db:"cogs"`
		MissingCosts int     `db:"missing_costs"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		WITH payment_sources AS (
		  SELECT p.id AS payment_id, s.id AS sale_id, s.payment_id AS root_payment_id,
		         p.payment_date, p.created_at, p.recognized_amount,
		         s.subtotal, s.total,
		         SUM(p.recognized_amount) OVER (
		           PARTITION BY s.id
		           ORDER BY p.payment_date, p.created_at, p.id
		           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
		         ) AS collected_after
		  FROM sales s
		  JOIN payments p ON p.id=s.payment_id OR p.parent_payment_id=s.payment_id
		  WHERE s.gym_id=? AND s.deleted_at IS NULL AND p.deleted_at IS NULL
		    AND ((p.id=s.payment_id AND p.concept='product')
		      OR (p.parent_payment_id=s.payment_id AND p.concept='balance_settlement'))
		),
		payment_lines AS (
		  SELECT ps.*, si.id AS line_id, si.product_id,
		         si.product_name_snapshot AS product_name, si.quantity, si.line_total,
		         CASE WHEN si.unit_cost_snapshot IS NOT NULL
		           THEN si.quantity*si.unit_cost_snapshot ELSE 0 END AS line_cost,
		         si.unit_cost_snapshot IS NULL AS missing_cost,
		         SUM(si.line_total) OVER (
		           PARTITION BY ps.payment_id ORDER BY si.id
		           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
		         ) AS revenue_cumulative,
		         SUM(CASE WHEN si.unit_cost_snapshot IS NOT NULL
		           THEN si.quantity*si.unit_cost_snapshot ELSE 0 END) OVER (
		           PARTITION BY ps.payment_id ORDER BY si.id
		           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
		         ) AS cost_cumulative,
		         SUM(CASE WHEN si.unit_cost_snapshot IS NOT NULL
		           THEN si.quantity*si.unit_cost_snapshot ELSE 0 END) OVER (
		           PARTITION BY ps.payment_id
		         ) AS cost_total
		  FROM payment_sources ps
		  JOIN sale_items si ON si.sale_id=ps.sale_id AND si.deleted_at IS NULL
		),
		payment_amounts AS (
		  SELECT *,
		         ROUND(CAST(collected_after AS REAL)*cost_total/NULLIF(total,0),0)
		           - ROUND(CAST(collected_after-recognized_amount AS REAL)*cost_total/NULLIF(total,0),0) AS event_cogs
		  FROM payment_lines
		),
		payment_events AS (
		  SELECT product_id, product_name,
		         CASE WHEN payment_id=root_payment_id THEN quantity ELSE 0 END AS quantity,
		         ROUND(CAST(recognized_amount AS REAL)*revenue_cumulative/NULLIF(subtotal,0),0)
		           - ROUND(CAST(recognized_amount AS REAL)*(revenue_cumulative-line_total)/NULLIF(subtotal,0),0) AS revenue,
		         CASE WHEN cost_total>0 THEN
		           ROUND(CAST(event_cogs AS REAL)*cost_cumulative/cost_total,0)
		             - ROUND(CAST(event_cogs AS REAL)*(cost_cumulative-line_cost)/cost_total,0)
		           ELSE 0 END AS cogs,
		         CASE WHEN missing_cost AND recognized_amount<>0 THEN 1 ELSE 0 END AS missing_cost
		  FROM payment_amounts
		  WHERE payment_date>=? AND payment_date<=?
		),
		refund_lines AS (
		  SELECT r.id AS refund_id, r.amount AS refund_amount,
		         r.amount+r.balance_cancelled AS economic_amount,
		         ri.id AS refund_item_id, ri.amount AS item_amount, ri.quantity,
		         ri.disposition, si.product_id, si.product_name_snapshot AS product_name,
		         CASE WHEN ri.disposition='returned_to_stock' AND si.unit_cost_snapshot IS NOT NULL
		           THEN ri.quantity*si.unit_cost_snapshot ELSE 0 END AS returned_cost,
		         CASE WHEN ri.disposition='returned_to_stock' AND si.unit_cost_snapshot IS NULL THEN 1 ELSE 0 END AS missing_cost,
		         SUM(ri.amount) OVER (
		           PARTITION BY r.id ORDER BY ri.id
		           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
		         ) AS revenue_cumulative,
		         SUM(CASE WHEN ri.disposition='returned_to_stock' AND si.unit_cost_snapshot IS NOT NULL
		           THEN ri.quantity*si.unit_cost_snapshot ELSE 0 END) OVER (
		           PARTITION BY r.id ORDER BY ri.id
		           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
		         ) AS cost_cumulative,
		         SUM(CASE WHEN ri.disposition='returned_to_stock' AND si.unit_cost_snapshot IS NOT NULL
		           THEN ri.quantity*si.unit_cost_snapshot ELSE 0 END) OVER (
		           PARTITION BY r.id
		         ) AS cost_total
		  FROM refund_items ri
		  JOIN refunds r ON r.id=ri.refund_id AND r.deleted_at IS NULL
		  JOIN sale_items si ON si.id=ri.sale_item_id AND si.deleted_at IS NULL
		  WHERE ri.gym_id=? AND ri.deleted_at IS NULL AND r.kind='revenue_refund'
		    AND r.refunded_on>=? AND r.refunded_on<=?
		),
		refund_amounts AS (
		  SELECT *, ROUND(CAST(refund_amount AS REAL)*cost_total/NULLIF(economic_amount,0),0) AS event_cogs
		  FROM refund_lines
		),
		refund_events AS (
		  SELECT product_id, product_name, -quantity AS quantity,
		         -(ROUND(CAST(refund_amount AS REAL)*revenue_cumulative/NULLIF(economic_amount,0),0)
		           - ROUND(CAST(refund_amount AS REAL)*(revenue_cumulative-item_amount)/NULLIF(economic_amount,0),0)) AS revenue,
		         CASE WHEN cost_total>0 THEN
		           -(ROUND(CAST(event_cogs AS REAL)*cost_cumulative/cost_total,0)
		             - ROUND(CAST(event_cogs AS REAL)*(cost_cumulative-returned_cost)/cost_total,0))
		           ELSE 0 END AS cogs,
		         missing_cost
		  FROM refund_amounts
		),
		events AS (
		  SELECT * FROM payment_events
		  UNION ALL
		  SELECT * FROM refund_events
		)
		SELECT product_id, MAX(product_name) AS product_name,
		       SUM(quantity) AS quantity, ROUND(SUM(revenue),0) AS revenue,
		       ROUND(SUM(cogs),0) AS cogs,
		       SUM(missing_cost) AS missing_costs
		FROM events
		GROUP BY product_id
		ORDER BY SUM(revenue) DESC, MAX(product_name), product_id`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	out := make([]reports.ProductProfitabilityRow, len(rows))
	for i, x := range rows {
		id, _ := uuid.Parse(x.ProductID)
		revenue := math.Round(x.RevenueCents) / 100
		cogs := math.Round(x.CogsCents) / 100
		profit := math.Round((revenue-cogs)*100) / 100
		out[i] = reports.ProductProfitabilityRow{
			ProductID: id, ProductName: x.ProductName, Quantity: x.Quantity,
			Revenue: revenue, COGS: cogs, GrossProfit: profit,
			CostComplete: x.MissingCosts == 0,
		}
		if revenue != 0 && x.MissingCosts == 0 {
			pct := math.Round(profit/revenue*10000) / 100
			out[i].MarginPct = &pct
		}
	}
	return out, nil
}

// RealizedProductProfitBetween — espejo SQLite de la base de caja de PG:
// cobros y abonos reconocen COGS proporcional con asignación acumulada de
// centavos; refunds negativos revierten costo sólo al regresar al stock.
func (r *SQLiteReader) RealizedProductProfitBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (reports.RealizedProductProfit, error) {
	rows, err := r.ProductProfitabilityBetween(tx, gymID, from, to)
	if err != nil {
		return reports.RealizedProductProfit{}, err
	}
	var out reports.RealizedProductProfit
	for _, row := range rows {
		out.Revenue += row.Revenue
		out.COGS += row.COGS
		out.ItemsTotal++
		if row.CostComplete {
			out.ItemsWithCost++
		}
	}
	out.Revenue = math.Round(out.Revenue*100) / 100
	out.COGS = math.Round(out.COGS*100) / 100
	return out, nil
}

// ListInventoryCostsBetween — JOINea product_name. ORDER BY created_at
// DESC para que el último egreso quede arriba en la tabla del FE.
func (r *SQLiteReader) ListInventoryCostsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time, limit int) ([]reports.InventoryCostRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit == 0 {
		limit = 200
	}
	type row struct {
		MovementID  string         `db:"movement_id"`
		ProductID   string         `db:"product_id"`
		ProductName string         `db:"product_name"`
		Delta       int            `db:"delta"`
		CostCents   int64          `db:"cost"`
		Reason      sql.NullString `db:"reason"`
		PaidOn      string         `db:"paid_on"`
	}
	var rows []row
	fromMs, toMs := dayBoundsMs(tzName, from, to)
	offset := tz.OffsetSeconds(tzName, from)
	query := `
		SELECT movement_id,product_id,product_name,delta,cost,reason,paid_on
		FROM (
		  SELECT COALESCE(ip.stock_movement_id,ip.id) AS movement_id,ip.product_id,p.name AS product_name,
		         ip.quantity AS delta,ip.unit_cost AS cost,sm.reason,ip.paid_on,ip.created_at AS sort_at
		  FROM inventory_purchases ip
		  JOIN products p ON p.id=ip.product_id
		  LEFT JOIN stock_movements sm ON sm.id=ip.stock_movement_id
		  WHERE ip.gym_id=? AND ip.deleted_at IS NULL AND ip.status='paid'
		    AND ip.paid_on>=? AND ip.paid_on<=?
		  UNION ALL
		  SELECT sm.id,sm.product_id,p.name,sm.delta,
		         COALESCE(NULLIF(ip.unit_cost,0),sm.cost),sm.reason,
		         ` + localDayExpr("sm.created_at") + ` AS paid_on,sm.created_at AS sort_at
		  FROM stock_movements sm
		  JOIN products p ON p.id=sm.product_id
		  LEFT JOIN inventory_purchases ip
		    ON ip.stock_movement_id=sm.id AND ip.deleted_at IS NULL
		  WHERE sm.gym_id=? AND sm.deleted_at IS NULL AND sm.movement_type='restock'
		    AND sm.is_purchase=1 AND sm.created_at>=? AND sm.created_at<? AND sm.delta>0
		    AND (ip.id IS NULL OR ip.status='legacy_incomplete')
		    AND COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost,0)>0
		) purchase_rows
		ORDER BY paid_on DESC,sort_at DESC`
	args := []any{gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		offset, gymID.String(), fromMs, toMs}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	if err := stx.Select(context.Background(), &rows, query, args...); err != nil {
		return nil, err
	}
	out := make([]reports.InventoryCostRow, 0, len(rows))
	for _, x := range rows {
		mid, _ := uuid.Parse(x.MovementID)
		pid, _ := uuid.Parse(x.ProductID)
		costUnit := float64(x.CostCents) / 100
		paidOn, _ := time.Parse(sqliteDateFmt, x.PaidOn)
		entry := reports.InventoryCostRow{
			MovementID:  mid,
			ProductID:   pid,
			ProductName: x.ProductName,
			Delta:       x.Delta,
			CostUnit:    costUnit,
			CostTotal:   costUnit * float64(x.Delta),
			OccurredAt:  paidOn,
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
	if limit == 0 {
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
	query := `
		SELECT id, expense_date, amount, category, description, payment_method
		FROM expenses
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND expense_date >= ? AND expense_date <= ?
		ORDER BY expense_date DESC, created_at DESC`
	args := []any{gymID.String(), from.Format("2006-01-02"), to.Format("2006-01-02")}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	if err := stx.Select(context.Background(), &rows, query, args...); err != nil {
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

// ExpensesDailySeries — egresos por día = gastos pagados (expense_date) +
// compras explícitas pagadas (paid_on) + devoluciones de ingreso
// (refunded_on). Suma en memoria para mantener la misma ecuación que el
// resultado canónico.
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

	type invRow struct {
		Day   string `db:"day"`
		Total int64  `db:"total"`
	}
	var invRows []invRow
	if err := stx.Select(context.Background(), &invRows, `
		SELECT paid_on AS day, COALESCE(SUM(total_amount), 0) AS total
		FROM inventory_purchases
		WHERE gym_id = ? AND deleted_at IS NULL AND status='paid'
		  AND paid_on >= ? AND paid_on <= ?
		GROUP BY paid_on`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt)); err != nil {
		return nil, err
	}
	for _, x := range invRows {
		bucket[x.Day] += x.Total
	}
	// Before the explicit Purchase aggregate existed, is_purchase=true was
	// the affirmative record that money left the gym. If cost survived, its
	// amount is exact; created_at is the best available economic date.
	fromMs, toMs := dayBoundsMs(tzName, from, to)
	type legacyInvRow struct {
		CreatedAt int64 `db:"created_at"`
		Total     int64 `db:"total"`
	}
	var legacyInvRows []legacyInvRow
	if err := stx.Select(context.Background(), &legacyInvRows, `
		SELECT sm.created_at,COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost) AS total
		FROM stock_movements sm
		LEFT JOIN inventory_purchases ip
		  ON ip.stock_movement_id=sm.id AND ip.deleted_at IS NULL
		WHERE sm.gym_id=? AND sm.deleted_at IS NULL AND sm.movement_type='restock'
		  AND sm.is_purchase=1 AND sm.created_at>=? AND sm.created_at<? AND sm.delta>0
		  AND (ip.id IS NULL OR ip.status='legacy_incomplete')
		  AND COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost,0)>0`,
		gymID.String(), fromMs, toMs); err != nil {
		return nil, err
	}
	loc := tz.LocationOrUTC(tzName)
	for _, x := range legacyInvRows {
		bucket[time.UnixMilli(x.CreatedAt).In(loc).Format(sqliteDateFmt)] += x.Total
	}

	type refRow struct {
		Day   string `db:"day"`
		Total int64  `db:"total"`
	}
	var refRows []refRow
	if err := stx.Select(context.Background(), &refRows, `
		SELECT refunded_on AS day, COALESCE(SUM(amount), 0) AS total
		FROM refunds
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND kind = 'revenue_refund'
		  AND refunded_on >= ? AND refunded_on <= ?
		GROUP BY refunded_on`,
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

// TopProductsBetween is the gross commercial ranking. Revenue and quantity
// use the same basis as the "Ventas de productos" KPI; refunds live in their
// own KPI. Net revenue/profit belongs to the Plus profitability analysis.
func (r *SQLiteReader) TopProductsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]reports.TopProductRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 {
		limit = 5
	}
	type row struct {
		ProductID   string  `db:"product_id"`
		ProductName string  `db:"product_name"`
		Quantity    int     `db:"quantity"`
		Revenue     float64 `db:"revenue"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		WITH units_sold AS (
		  SELECT si.product_id, SUM(si.quantity) AS quantity
		  FROM sale_items si
		  JOIN sales s ON s.id = si.sale_id AND s.deleted_at IS NULL
		  JOIN payments p ON p.id = s.payment_id AND p.deleted_at IS NULL AND p.concept = 'product'
		  WHERE si.gym_id = ? AND si.deleted_at IS NULL
		    AND p.payment_date >= ? AND p.payment_date <= ?
		  GROUP BY product_id
		), revenue_events AS (
		  SELECT si.product_id,
		         CAST(p.recognized_amount AS REAL) * si.line_total / NULLIF(s.subtotal, 0) AS amount
		  FROM sale_items si
		  JOIN sales s ON s.id = si.sale_id AND s.deleted_at IS NULL
		  JOIN payments p ON p.id = s.payment_id OR p.parent_payment_id = s.payment_id
		  WHERE si.gym_id = ? AND si.deleted_at IS NULL AND p.deleted_at IS NULL
		    AND p.payment_date >= ? AND p.payment_date <= ?
		    AND ((p.id = s.payment_id AND p.concept = 'product')
		      OR (p.parent_payment_id = s.payment_id AND p.concept = 'balance_settlement'))
		), cash_revenue AS (
		  SELECT product_id, SUM(amount) AS revenue FROM revenue_events
		  GROUP BY product_id
		)
		SELECT pr.id AS product_id, pr.name AS product_name,
		       COALESCE(u.quantity, 0) AS quantity,
		       COALESCE(c.revenue, 0) AS revenue
		FROM products pr
		LEFT JOIN units_sold u ON u.product_id = pr.id
		LEFT JOIN cash_revenue c ON c.product_id = pr.id
		WHERE pr.gym_id = ? AND pr.deleted_at IS NULL
		  AND (u.product_id IS NOT NULL OR c.product_id IS NOT NULL)
		ORDER BY revenue DESC
		LIMIT ?`,
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), from.Format(sqliteDateFmt), to.Format(sqliteDateFmt),
		gymID.String(), limit); err != nil {
		return nil, err
	}
	out := make([]reports.TopProductRow, 0, len(rows))
	for _, x := range rows {
		pid, _ := uuid.Parse(x.ProductID)
		out = append(out, reports.TopProductRow{
			ProductID:   pid,
			ProductName: x.ProductName,
			Quantity:    x.Quantity,
			Revenue:     x.Revenue / 100,
		})
	}
	return out, nil
}

// SumProductSalesBetween — espejo del PG: $ por cobro inicial + abonos de
// producto (INTEGER cents → /100), unidades por ventas originadas.
func (r *SQLiteReader) SumProductSalesBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (reports.ProductSalesTotals, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var cents sql.NullInt64
	if err := stx.Get(context.Background(), &cents, `
		SELECT COALESCE(SUM(recognized_amount), 0) FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND (concept = 'product' OR (concept = 'balance_settlement' AND EXISTS (
		    SELECT 1 FROM sales s WHERE s.payment_id = payments.parent_payment_id AND s.deleted_at IS NULL
		  )))
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
		    COALESCE(SUM(CASE WHEN stock <= 0 THEN 1 ELSE 0 END), 0) AS out_count,
		    COALESCE(SUM(CASE WHEN stock > 0 AND stock <= stock_minimum THEN 1 ELSE 0 END), 0) AS low_count
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
		    COALESCE(SUM(CASE WHEN m.gender = 'hombre' THEN 1 ELSE 0 END), 0)                                AS hombre,
		    COALESCE(SUM(CASE WHEN m.gender = 'mujer' THEN 1 ELSE 0 END), 0)                                 AS mujer,
		    COALESCE(SUM(CASE WHEN m.gender IS NULL OR m.gender = 'no_especificado' THEN 1 ELSE 0 END), 0)   AS no_especificado,
		    COUNT(*)                                                                            AS total
		FROM members m
		WHERE m.gym_id = ?
		  AND m.status = 'active'
		  AND m.deleted_at IS NULL
		  AND EXISTS (
		      SELECT 1 FROM memberships ms
		      WHERE ms.member_id = m.id AND ms.deleted_at IS NULL
		        AND ms.status IN ('active', 'replaced')
		        AND ms.start_date <= ? AND ms.expiry_date >= ?
		  )`,
		gymID.String(), today.Format(sqliteDateFmt), today.Format(sqliteDateFmt))
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
