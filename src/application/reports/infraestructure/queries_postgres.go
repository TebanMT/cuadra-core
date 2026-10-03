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
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/application/reports"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// PostgresReader implements reports.Reader against PG via GORM.
type PostgresReader struct{}

func NewPostgresReader() *PostgresReader { return &PostgresReader{} }

const dateFmt = "2006-01-02"

// FinancialReadMetadata returns the newest mutation that can change a money
// total or cash reconciliation. Deleted rows stay in scope because a
// tombstone is itself a financial change. The cloud deliberately returns a
// null SyncPending: only a sidecar can inspect its unsent local queue.
func (r *PostgresReader) FinancialReadMetadata(tx sharedDomain.Transaction, gymID uuid.UUID) (reports.FinancialReadMetadata, error) {
	type row struct {
		Watermark *time.Time
	}
	var value row
	err := tx.(*sharedDomain.GormTransaction).Tx.Raw(`
		SELECT MAX(updated_at) AS watermark FROM (
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
		) financial_events`, gymID, gymID, gymID, gymID, gymID, gymID, gymID, gymID, gymID, gymID, gymID, gymID).Scan(&value).Error
	if err != nil {
		return reports.FinancialReadMetadata{}, err
	}
	if value.Watermark != nil {
		v := value.Watermark.UTC()
		value.Watermark = &v
	}
	return reports.FinancialReadMetadata{DataWatermark: value.Watermark, SyncPending: nil}, nil
}

// ---------------------------------------------------------------------------
// Dashboard KPIs
// ---------------------------------------------------------------------------

func (r *PostgresReader) CountActiveMembers(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Raw(`
		SELECT COUNT(*) FROM members m
		WHERE m.gym_id = ?
		  AND m.status = 'active'
		  AND m.deleted_at IS NULL
		  AND EXISTS (
		      SELECT 1 FROM memberships ms
		      WHERE ms.member_id = m.id AND ms.deleted_at IS NULL
		        AND ms.status IN ('active', 'replaced')
		        AND ms.start_date <= ? AND ms.expiry_date >= ?
		  )`, gymID, today.Format(dateFmt), today.Format(dateFmt)).Scan(&n).Error
	return int(n), err
}

func (r *PostgresReader) SumPaymentsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var total float64
	err := gormTx.Raw(`
		SELECT COALESCE(SUM(recognized_amount), 0) FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept <> 'refund'
		  AND payment_date >= ? AND payment_date <= ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&total).Error
	return total, err
}

func (r *PostgresReader) SumOtherIncomeBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var total float64
	err := gormTx.Raw(`
		SELECT COALESCE(SUM(recognized_amount), 0) FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND concept = 'other'
		  AND payment_date >= ? AND payment_date <= ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&total).Error
	return total, err
}

func (r *PostgresReader) SumCashClosedBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var total float64
	err := gormTx.Raw(`
		SELECT
		  COALESCE((SELECT SUM(p.amount) FROM payments p
		    WHERE p.gym_id = ? AND p.deleted_at IS NULL AND p.payment_method = 'cash' AND p.amount<>0 AND p.cash_destination<>'gym_fund'
		      AND p.payment_date >= ? AND p.payment_date <= ?), 0)
		  + COALESCE((SELECT SUM(CASE WHEN m.movement_type = 'cash_in' THEN m.amount ELSE -m.amount END)
		    FROM cash_movements m
		    WHERE m.gym_id = ? AND m.deleted_at IS NULL
		      AND m.movement_on >= ? AND m.movement_on <= ?), 0)`,
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&total).Error
	return total, err
}

func (r *PostgresReader) SumCashCountedBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (reports.CashCountSummary, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row struct {
		Counted                         float64    `gorm:"column:counted"`
		CountedCloses                   int        `gorm:"column:counted_closes"`
		TotalCloses                     int        `gorm:"column:total_closes"`
		Withdrawn                       float64    `gorm:"column:withdrawn"`
		HistoricalActivityDays          int        `gorm:"column:historical_activity_days"`
		HistoricalSessions              int        `gorm:"column:historical_sessions"`
		ActiveDays                      int        `gorm:"column:active_days"`
		MissingActiveDays               int        `gorm:"column:missing_active_days"`
		UncoveredActivityDays           int        `gorm:"column:uncovered_activity_days"`
		OpenSessions                    int        `gorm:"column:open_sessions"`
		ClosedUnverifiedSessions        int        `gorm:"column:closed_unverified_sessions"`
		ReconciledSessions              int        `gorm:"column:reconciled_sessions"`
		StaleSessions                   int        `gorm:"column:stale_sessions"`
		WithdrawnSessions               int        `gorm:"column:withdrawn_sessions"`
		UnknownOpeningSessions          int        `gorm:"column:unknown_opening_sessions"`
		AdjustedAfterWithdrawalSessions int        `gorm:"column:adjusted_after_withdrawal_sessions"`
		LegacyCashSourceUnverifiedCount int        `gorm:"column:legacy_cash_source_unverified_count"`
		TotalSessions                   int        `gorm:"column:total_sessions"`
		LatestSessionID                 *uuid.UUID `gorm:"column:latest_session_id"`
		LatestExpected                  *float64   `gorm:"column:latest_expected"`
		LatestCounted                   *float64   `gorm:"column:latest_counted"`
		LatestDifference                *float64   `gorm:"column:latest_difference"`
		LatestCountedAt                 *time.Time `gorm:"column:latest_counted_at"`
		LatestStatus                    *string    `gorm:"column:latest_status"`
		LatestNeedsRecount              bool       `gorm:"column:latest_needs_recount"`
	}
	err := gormTx.Raw(`
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
		  FROM gym_sessions WHERE opening_cash_known=TRUE
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
		    (s.withdrawn_at IS NOT NULL
		      AND ca.amount<>COALESCE(s.activity_cash,0)) AS dynamic_post_withdrawal_adjustment,
		    CASE WHEN s.status='stale' THEN TRUE
		      WHEN COALESCE(s.status,'closed_unverified') IN ('open','withdrawn') THEN FALSE
		      WHEN ca.amount<>COALESCE(s.activity_cash,0) THEN TRUE
		      WHEN s.closed_at IS NOT NULL AND EXISTS (
		        SELECT 1 FROM physical p
		        WHERE p.day=COALESCE(s.operational_date,s.close_date)
		          AND p.drawer_id=COALESCE(s.drawer_id,s.gym_id)
		          AND p.recorded_at>=COALESCE(s.opened_at,s.created_at)
		          AND p.recorded_at>s.closed_at
		          AND (s.withdrawn_at IS NULL OR p.recorded_at<=s.withdrawn_at)
		      ) THEN TRUE ELSE FALSE END AS needs_recount
		  FROM scoped_base s JOIN current_activity ca ON ca.id=s.id
		), effective AS (
		  SELECT s.*,
		    CASE WHEN needs_recount THEN 'stale'
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
		       (SELECT COUNT(*) FROM effective WHERE NOT COALESCE(opening_cash_known,false)) AS unknown_opening_sessions,
		       (SELECT COUNT(*) FROM effective
		         WHERE COALESCE(adjusted_after_withdrawal,false)
		            OR dynamic_post_withdrawal_adjustment) AS adjusted_after_withdrawal_sessions,
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
		                  AND NOT COALESCE(l.adjusted_after_withdrawal,false)
		                  AND NOT l.dynamic_post_withdrawal_adjustment
		            THEN l.counted_cash-l.calculated_cash END AS latest_difference,
		       CASE WHEN l.counted_cash IS NOT NULL
		            THEN COALESCE(l.reconciled_at,l.closed_at,l.updated_at) END AS latest_counted_at,
		       l.effective_status AS latest_status,
		       (COALESCE(l.needs_recount,false)
		         OR COALESCE(l.dynamic_post_withdrawal_adjustment,false)
		         OR COALESCE(l.adjusted_after_withdrawal,false)) AS latest_needs_recount
		FROM (SELECT 1) anchor LEFT JOIN latest l ON true`,
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&row).Error
	out := reports.CashCountSummary{
		Counted: row.Counted, CountedCloses: row.CountedCloses, TotalCloses: row.TotalCloses,
		Withdrawn: row.Withdrawn, LatestSessionID: row.LatestSessionID,
		HistoricalActivityDays: row.HistoricalActivityDays, HistoricalSessions: row.HistoricalSessions,
		ActiveDays: row.ActiveDays, MissingActiveDays: row.MissingActiveDays,
		UncoveredActivityDays: row.UncoveredActivityDays, OpenSessions: row.OpenSessions,
		ClosedUnverifiedSessions: row.ClosedUnverifiedSessions,
		ReconciledSessions:       row.ReconciledSessions, StaleSessions: row.StaleSessions,
		WithdrawnSessions: row.WithdrawnSessions, UnknownOpeningSessions: row.UnknownOpeningSessions,
		AdjustedAfterWithdrawalSessions: row.AdjustedAfterWithdrawalSessions,
		LegacyCashSourceUnverifiedCount: row.LegacyCashSourceUnverifiedCount,
		TotalSessions:                   row.TotalSessions,
		LatestExpected:                  row.LatestExpected, LatestCounted: row.LatestCounted,
		LatestDifference: row.LatestDifference, LatestCountedAt: row.LatestCountedAt,
		LatestNeedsRecount: row.LatestNeedsRecount,
	}
	if row.LatestStatus != nil {
		out.LatestStatus = *row.LatestStatus
	}
	return out, err
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
		  AND ms.start_date <= ?
		  AND ms.expiry_date >= ? AND ms.expiry_date <= ?`,
		gymID, from.Format(dateFmt), from.Format(dateFmt), to.Format(dateFmt)).Scan(&n).Error
	return int(n), err
}

func (r *PostgresReader) CountExpiredRecoverable(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time, withinDays int) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	cutoff := today.AddDate(0, 0, -withinDays)
	var n int64
	err := gormTx.Raw(`
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
		  )`, gymID, today.Format(dateFmt), cutoff.Format(dateFmt),
		today.Format(dateFmt), today.Format(dateFmt)).Scan(&n).Error
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
		SELECT payment_date AS day, COALESCE(SUM(recognized_amount), 0) AS total
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
		  AND ms.start_date <= ?
		  AND ms.expiry_date >= ? AND ms.expiry_date <= ?
		ORDER BY ms.expiry_date ASC`,
		gymID, today.Format(dateFmt), today.Format(dateFmt), endDate.Format(dateFmt)).Scan(&rows).Error; err != nil {
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
		JOIN LATERAL (
		    SELECT expired.expiry_date, expired.type_name_snapshot
		    FROM memberships expired
		    WHERE expired.member_id = m.id AND expired.deleted_at IS NULL
		      AND expired.expiry_date < ? AND expired.expiry_date >= ?
		    ORDER BY expired.expiry_date DESC, expired.created_at DESC
		    LIMIT 1
		) ms ON TRUE
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
		today.Format(dateFmt), cutoff.Format(dateFmt), gymID, staleAfter,
		today.Format(dateFmt), today.Format(dateFmt)).Scan(&rows).Error; err != nil {
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
		gymID, today.Format(dateFmt), today.Format(dateFmt), cutoff).Scan(&rows).Error; err != nil {
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
	// Deuda viva TOTAL por socio: SUM(balance_pending) de todos sus pagos
	// vivos — mismo predicado que billing.SumPendingByMember (el total del
	// perfil del socio), para que esta lista y ese número nunca discrepen.
	// La versión anterior tomaba sólo el pago MÁS RECIENTE (DISTINCT ON):
	// un pago nuevo saldado escondía la deuda vieja, y deudas repartidas
	// en varios pagos mostraban sólo la última. balance_pending > 0
	// pre-GROUP basta porque el dominio nunca produce balances negativos,
	// y deja que MIN(payment_date) sea la deuda abierta más vieja (el
	// "debe desde" del wire).
	if err := gormTx.Raw(`
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

func (r *PostgresReader) ListSalesForExport(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) ([]reports.SaleExportRow, error) {
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
	// Ventas del día LOCAL del gym: created_at es un instante y las cotas
	// eran medianoche UTC, así que las ventas de la tarde se exportaban en
	// el día siguiente.
	salesStart, salesEnd := tz.DayBounds(tzName, from, to)
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
		gymID, salesStart, salesEnd).Scan(&rows).Error; err != nil {
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

func (r *PostgresReader) CountNewMembersBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	// Calendar-day window: members whose registration falls in [from..to]
	// según el día LOCAL del gym. created_at es TIMESTAMPTZ (un instante),
	// así que traducimos los días a un rango de instantes en vez de
	// truncar la columna: `created_at::date` daba el día UTC y además
	// impedía usar índice.
	start, end := tz.DayBounds(tzName, from, to)
	err := gormTx.Raw(`
		SELECT COUNT(*) FROM members
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND created_at >= ? AND created_at < ?`,
		gymID, start, end).Scan(&n).Error
	return int(n), err
}

func (r *PostgresReader) CountCheckinsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	// Rango de instantes del día local del gym (ver tz.DayBounds). La
	// columna va desnuda a propósito: así el planner puede usar
	// idx_checkins_gym_date, que `checkin_at::date` inutilizaba.
	start, end := tz.DayBounds(tzName, from, to)
	err := gormTx.Raw(`
		SELECT COUNT(*) FROM checkins
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND result LIKE 'allowed%'
		  AND checkin_at >= ? AND checkin_at < ?`,
		gymID, start, end).Scan(&n).Error
	return int(n), err
}

func (r *PostgresReader) SumRefundsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var total float64
	// Refund aggregates store a positive magnitude. Only revenue_refund is a
	// canonical outflow; overcollection settlements move money but do not
	// subtract income a second time.
	err := gormTx.Raw(`
		SELECT COALESCE(SUM(amount), 0) FROM refunds
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND kind = 'revenue_refund'
		  AND refunded_on >= ? AND refunded_on <= ?`,
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
		SELECT payment_method AS method, COALESCE(SUM(recognized_amount), 0) AS total
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

// IncomeByMembershipTypeBetween incluye el cobro inicial y sus abonos. Los
// atribuye primero a la membresía exacta guardada en el pago; sólo los pagos
// legacy sin membership_id recurren a inferirla por socio y fecha.
func (r *PostgresReader) IncomeByMembershipTypeBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (map[string]float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		TypeName string
		Total    float64
	}
	var rows []row
	if err := gormTx.Raw(`
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
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, x := range rows {
		out[x.TypeName] = x.Total
	}
	return out, nil
}

// ActiveMembersByType — mismo predicado que CountActiveMembers, agrupado,
// para que SUM(buckets) == ese KPI siempre.
func (r *PostgresReader) ActiveMembersByType(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (map[string]int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		TypeName string
		N        int
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT current_ms.type_name_snapshot AS type_name, COUNT(*) AS n
		FROM members m
		JOIN LATERAL (
		    SELECT ms.type_name_snapshot
		    FROM memberships ms
		    WHERE ms.member_id = m.id AND ms.deleted_at IS NULL
		      AND ms.status IN ('active', 'replaced')
		      AND ms.start_date <= ? AND ms.expiry_date >= ?
		    ORDER BY ms.expiry_date DESC,
		             CASE WHEN ms.status = 'active' THEN 0 ELSE 1 END,
		             ms.created_at DESC
		    LIMIT 1
		) current_ms ON TRUE
		WHERE m.gym_id = ?
		  AND m.status = 'active'
		  AND m.deleted_at IS NULL
		GROUP BY 1`,
		today.Format(dateFmt), today.Format(dateFmt), gymID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, x := range rows {
		out[x.TypeName] = x.N
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
		gymID, from.Format(dateFmt), to.Format(dateFmt), limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.TopMemberRow, len(rows))
	for i, x := range rows {
		out[i] = reports.TopMemberRow(x)
	}
	return out, nil
}

func (r *PostgresReader) CheckinsDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) ([]reports.DailyCount, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		Day   time.Time
		Count int
	}
	// Única query del grupo donde SÍ hay que convertir la columna: agrupar
	// por día local exige clasificar CADA fila, no sólo acotar el rango.
	// El WHERE sigue con la columna desnuda (usa índice) y el AT TIME ZONE
	// sólo corre sobre las filas ya filtradas.
	start, end := tz.DayBounds(tzName, from, to)
	zone := tz.NameOrUTC(tzName)
	var rows []row
	if err := gormTx.Raw(`
		SELECT (checkin_at AT TIME ZONE ?)::date AS day, COUNT(*) AS count
		FROM checkins
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND result LIKE 'allowed%'
		  AND checkin_at >= ? AND checkin_at < ?
		GROUP BY 1
		ORDER BY 1`,
		zone, gymID, start, end).Scan(&rows).Error; err != nil {
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
		SaleSummary *string
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT p.id, p.member_id,
		       m.full_name AS member_name,
		       p.amount, p.payment_method AS method, p.concept,
		       p.payment_date,
		       (SELECT string_agg(
		          si.product_name_snapshot ||
		            CASE WHEN si.quantity > 1 THEN ' ×' || si.quantity ELSE '' END,
		          ' · ' ORDER BY si.line_total DESC)
		        FROM sales s
		        JOIN sale_items si ON si.sale_id = s.id AND si.deleted_at IS NULL
		        WHERE s.payment_id = p.id AND s.deleted_at IS NULL) AS sale_summary
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

// SumInventoryCostBetween suma compras explícitas pagadas y el monto exacto
// de compras históricas anteriores al agregado Purchase cuando el journal
// todavía conserva cantidad y costo unitario.
func (r *PostgresReader) SumInventoryCostBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	start, end := tz.DayBounds(tzName, from, to)
	var total float64
	err := gormTx.Raw(`
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
		    AND sm.movement_type='restock' AND sm.is_purchase=TRUE
		    AND sm.created_at>=? AND sm.created_at<? AND sm.delta>0
		    AND (ip.id IS NULL OR ip.status='legacy_incomplete')
		    AND COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost,0)>0
		) purchases`,
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, start, end).Scan(&total).Error
	return total, err
}

// CanonicalFinancialBetween computes the complete Standard business summary
// in one database snapshot. Historical purchase movements contribute only
// when quantity × unit cost reconstructs an exact amount.
func (r *PostgresReader) CanonicalFinancialBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) (reports.CanonicalFinancialSnapshot, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	start, end := tz.DayBounds(tzName, from, to)
	var row struct {
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
		LegacyCashSourceUnverifiedCount  int
		LegacyPurchaseCount              int
		LegacyRefundCount                int
	}
	err := gormTx.Raw(`
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
		      AND sm.is_purchase=TRUE AND sm.created_at>=? AND sm.created_at<? AND sm.delta>0
		      AND (ip.id IS NULL OR ip.status='legacy_incomplete')
		      AND COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost,0)>0
		  ) purchases) AS inventory_purchases,
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
		      AND sm.is_purchase=TRUE AND sm.created_at>=? AND sm.created_at<?
		      AND (ip.id IS NULL OR ip.status='legacy_incomplete')
		      AND COALESCE(NULLIF(ip.total_amount,0),
		        CASE WHEN sm.delta>0 AND sm.cost>0 THEN sm.delta*sm.cost END,0)<=0) AS legacy_purchase_count,
		  (SELECT COUNT(*) FROM payments rp
		    LEFT JOIN refunds rr ON rr.refund_payment_id=rp.id AND rr.deleted_at IS NULL
		    WHERE rp.gym_id=? AND rp.deleted_at IS NULL AND rp.concept='refund'
		      AND rp.payment_date>=? AND rp.payment_date<=?
		      AND (rr.id IS NULL OR rr.legacy_incomplete=TRUE)) AS legacy_refund_count
		FROM classified`,
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, start, end,
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, start, end,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&row).Error
	if err != nil {
		return reports.CanonicalFinancialSnapshot{}, err
	}
	return reports.CanonicalFinancialSnapshot(row), nil
}

func (r *PostgresReader) ProductProfitabilityBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) ([]reports.ProductProfitabilityRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		ProductID    uuid.UUID
		ProductName  string
		Quantity     int
		Revenue      float64
		Cogs         float64
		MissingCosts int
	}
	var rows []row
	if err := gormTx.Raw(`
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
		         ROUND(collected_after*cost_total/NULLIF(total,0),2)
		           - ROUND((collected_after-recognized_amount)*cost_total/NULLIF(total,0),2) AS event_cogs
		  FROM payment_lines
		),
		payment_events AS (
		  SELECT product_id, product_name,
		         CASE WHEN payment_id=root_payment_id THEN quantity ELSE 0 END AS quantity,
		         ROUND(recognized_amount*revenue_cumulative/NULLIF(subtotal,0),2)
		           - ROUND(recognized_amount*(revenue_cumulative-line_total)/NULLIF(subtotal,0),2) AS revenue,
		         CASE WHEN cost_total>0 THEN
		           ROUND(event_cogs*cost_cumulative/cost_total,2)
		             - ROUND(event_cogs*(cost_cumulative-line_cost)/cost_total,2)
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
		  SELECT *, ROUND(refund_amount*cost_total/NULLIF(economic_amount,0),2) AS event_cogs
		  FROM refund_lines
		),
		refund_events AS (
		  SELECT product_id, product_name, -quantity AS quantity,
		         -(ROUND(refund_amount*revenue_cumulative/NULLIF(economic_amount,0),2)
		           - ROUND(refund_amount*(revenue_cumulative-item_amount)/NULLIF(economic_amount,0),2)) AS revenue,
		         CASE WHEN cost_total>0 THEN
		           -(ROUND(event_cogs*cost_cumulative/cost_total,2)
		             - ROUND(event_cogs*(cost_cumulative-returned_cost)/cost_total,2))
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
		       SUM(quantity)::int AS quantity,
		       ROUND(SUM(revenue),2) AS revenue,
		       ROUND(SUM(cogs),2) AS cogs,
		       SUM(missing_cost)::int AS missing_costs
		FROM events
		GROUP BY product_id
		ORDER BY SUM(revenue) DESC, MAX(product_name), product_id`,
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]reports.ProductProfitabilityRow, len(rows))
	for i, x := range rows {
		profit := roundReportMoney(x.Revenue - x.Cogs)
		out[i] = reports.ProductProfitabilityRow{
			ProductID: x.ProductID, ProductName: x.ProductName, Quantity: x.Quantity,
			Revenue: roundReportMoney(x.Revenue), COGS: roundReportMoney(x.Cogs),
			GrossProfit: profit, CostComplete: x.MissingCosts == 0,
		}
		if x.Revenue != 0 && x.MissingCosts == 0 {
			pct := math.Round(profit/x.Revenue*10000) / 100
			out[i].MarginPct = &pct
		}
	}
	return out, nil
}

func roundReportMoney(v float64) float64 { return math.Round(v*100) / 100 }

// RealizedProductProfitBetween — base de caja. Cada cobro inicial/abono
// reconoce COGS de forma acumulada y asigna cada centavo una sola vez. Un
// refund revierte revenue; sólo returned_to_stock revierte COGS, mientras
// damaged conserva el costo como pérdida. Así venta fiada, descuentos y
// devoluciones parciales cuadran exactamente con el dinero registrado.
func (r *PostgresReader) RealizedProductProfitBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (reports.RealizedProductProfit, error) {
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
	out.Revenue = roundReportMoney(out.Revenue)
	out.COGS = roundReportMoney(out.COGS)
	return out, nil
}

// ListInventoryCostsBetween — JOIN a products para incluir el nombre y
// evitar el N+1 desde el FE. ORDER DESC porque la tabla del FE muestra
// el último egreso arriba.
func (r *PostgresReader) ListInventoryCostsBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time, limit int) ([]reports.InventoryCostRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit == 0 {
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
	// Mismo criterio que SumInventoryCostBetween — la tabla y el total del
	// KPI tienen que cubrir exactamente las mismas filas.
	var rows []row
	start, end := tz.DayBounds(tzName, from, to)
	zone := tz.NameOrUTC(tzName)
	query := `
		SELECT movement_id,product_id,product_name,delta,cost,reason,occurred_on::timestamp AS created_at
		FROM (
		  SELECT COALESCE(ip.stock_movement_id,ip.id) AS movement_id,ip.product_id,p.name AS product_name,
		         ip.quantity AS delta,ip.unit_cost AS cost,sm.reason,ip.paid_on AS occurred_on,
		         ip.created_at AS sort_at
		  FROM inventory_purchases ip
		  JOIN products p ON p.id=ip.product_id
		  LEFT JOIN stock_movements sm ON sm.id=ip.stock_movement_id
		  WHERE ip.gym_id=? AND ip.deleted_at IS NULL AND ip.status='paid'
		    AND ip.paid_on>=? AND ip.paid_on<=?
		  UNION ALL
		  SELECT sm.id,sm.product_id,p.name,sm.delta,COALESCE(NULLIF(ip.unit_cost,0),sm.cost),sm.reason,
		         (sm.created_at AT TIME ZONE ?)::date AS occurred_on,sm.created_at AS sort_at
		  FROM stock_movements sm
		  JOIN products p ON p.id=sm.product_id
		  LEFT JOIN inventory_purchases ip
		    ON ip.stock_movement_id=sm.id AND ip.deleted_at IS NULL
		  WHERE sm.gym_id=? AND sm.deleted_at IS NULL AND sm.movement_type='restock'
		    AND sm.is_purchase=TRUE AND sm.created_at>=? AND sm.created_at<? AND sm.delta>0
		    AND (ip.id IS NULL OR ip.status='legacy_incomplete')
		    AND COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost,0)>0
		) purchase_rows
		ORDER BY occurred_on DESC,sort_at DESC`
	args := []any{gymID, from.Format(dateFmt), to.Format(dateFmt), zone, gymID, start, end}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	if err := gormTx.Raw(query, args...).Scan(&rows).Error; err != nil {
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
	if limit == 0 {
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
	query := `
		SELECT id, expense_date, amount, category, description, payment_method
		FROM expenses
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND expense_date >= ? AND expense_date <= ?
		ORDER BY expense_date DESC, created_at DESC`
	args := []any{gymID, from.Format(dateFmt), to.Format(dateFmt)}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	if err := gormTx.Raw(query, args...).Scan(&rows).Error; err != nil {
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

// ExpensesDailySeries — total egresado por día sumando gastos pagados,
// compras explícitas pagadas y devoluciones reales de ingreso.
// Hacemos las queries por separado y mergeamos en memoria (los gyms tienen
// pocos días por ventana — O(días) keys), evita un UNION complejo y
// mantiene el filtro por fecha homogéneo entre las fuentes. La serie usa
// las mismas fuentes y fechas económicas que el resultado canónico.
func (r *PostgresReader) ExpensesDailySeries(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time) ([]reports.DailyAmount, error) {
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
		SELECT paid_on AS day, COALESCE(SUM(total_amount), 0) AS total
		FROM inventory_purchases
		WHERE gym_id = ? AND deleted_at IS NULL AND status='paid'
		  AND paid_on >= ? AND paid_on <= ?
		GROUP BY paid_on`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&invRows).Error; err != nil {
		return nil, err
	}
	for _, x := range invRows {
		bucket[x.Day.Format(dateFmt)] += x.Total
	}
	start, end := tz.DayBounds(tzName, from, to)
	type legacyInvRow struct {
		CreatedAt time.Time
		Total     float64
	}
	var legacyInvRows []legacyInvRow
	if err := gormTx.Raw(`
		SELECT sm.created_at,COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost) AS total
		FROM stock_movements sm
		LEFT JOIN inventory_purchases ip
		  ON ip.stock_movement_id=sm.id AND ip.deleted_at IS NULL
		WHERE sm.gym_id=? AND sm.deleted_at IS NULL AND sm.movement_type='restock'
		  AND sm.is_purchase=TRUE AND sm.created_at>=? AND sm.created_at<? AND sm.delta>0
		  AND (ip.id IS NULL OR ip.status='legacy_incomplete')
		  AND COALESCE(NULLIF(ip.total_amount,0),sm.delta*sm.cost,0)>0`,
		gymID, start, end).Scan(&legacyInvRows).Error; err != nil {
		return nil, err
	}
	loc := tz.LocationOrUTC(tzName)
	for _, x := range legacyInvRows {
		bucket[x.CreatedAt.In(loc).Format(dateFmt)] += x.Total
	}

	var refRows []row
	if err := gormTx.Raw(`
		SELECT refunded_on AS day, COALESCE(SUM(amount), 0) AS total
		FROM refunds
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND kind = 'revenue_refund'
		  AND refunded_on >= ? AND refunded_on <= ?
		GROUP BY refunded_on`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&refRows).Error; err != nil {
		return nil, err
	}
	for _, x := range refRows {
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
// incluye compras de mercancía (eso vive en inventory_purchases y se
// reporta como "Compras de inventario" aparte).
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

// TopProductsBetween is the gross commercial ranking. Revenue and quantity
// use the same basis as the "Ventas de productos" KPI; refunds live in their
// own KPI. Net revenue/profit belongs to the Plus profitability analysis.
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
		WITH units_sold AS (
		  SELECT si.product_id, MIN(si.product_name_snapshot) AS product_name,
		         SUM(si.quantity) AS quantity
		  FROM sale_items si
		  JOIN sales s ON s.id = si.sale_id AND s.deleted_at IS NULL
		  JOIN payments p ON p.id = s.payment_id AND p.deleted_at IS NULL AND p.concept = 'product'
		  WHERE si.gym_id = ? AND si.deleted_at IS NULL
		    AND p.payment_date >= ? AND p.payment_date <= ?
		  GROUP BY product_id
		), revenue_events AS (
		  SELECT si.product_id, si.product_name_snapshot AS product_name,
		         p.recognized_amount * si.line_total / NULLIF(s.subtotal, 0) AS amount
		  FROM sale_items si
		  JOIN sales s ON s.id = si.sale_id AND s.deleted_at IS NULL
		  JOIN payments p ON p.id = s.payment_id OR p.parent_payment_id = s.payment_id
		  WHERE si.gym_id = ? AND si.deleted_at IS NULL AND p.deleted_at IS NULL
		    AND p.payment_date >= ? AND p.payment_date <= ?
		    AND ((p.id = s.payment_id AND p.concept = 'product')
		      OR (p.parent_payment_id = s.payment_id AND p.concept = 'balance_settlement'))
		), cash_revenue AS (
		  SELECT product_id, MIN(product_name) AS product_name, SUM(amount) AS revenue
		  FROM revenue_events
		  GROUP BY product_id
		)
		SELECT COALESCE(c.product_id, u.product_id) AS product_id,
		       COALESCE(c.product_name, u.product_name) AS product_name,
		       COALESCE(u.quantity, 0) AS quantity,
		       COALESCE(c.revenue, 0) AS revenue
		FROM cash_revenue c
		FULL OUTER JOIN units_sold u ON u.product_id = c.product_id
		ORDER BY revenue DESC
		LIMIT ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		gymID, from.Format(dateFmt), to.Format(dateFmt),
		limit).Scan(&rows).Error; err != nil {
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

// SumProductSalesBetween — $ y unidades de productos del período. El $ va
// directo por pagos de producto + sus abonos (cash-based por payment_date,
// mismo criterio que SumPaymentsBetween); las
// unidades por sale_items de esos mismos pagos. Refunds no restan aquí:
// viven en el KPI de devoluciones.
func (r *PostgresReader) SumProductSalesBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time) (reports.ProductSalesTotals, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var out reports.ProductSalesTotals
	if err := gormTx.Raw(`
		SELECT COALESCE(SUM(recognized_amount), 0) FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND (concept = 'product' OR (concept = 'balance_settlement' AND EXISTS (
		    SELECT 1 FROM sales s WHERE s.payment_id = payments.parent_payment_id AND s.deleted_at IS NULL
		  )))
		  AND payment_date >= ? AND payment_date <= ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&out.Amount).Error; err != nil {
		return reports.ProductSalesTotals{}, err
	}
	if err := gormTx.Raw(`
		SELECT COALESCE(SUM(si.quantity), 0)
		FROM sale_items si
		JOIN sales s ON s.id = si.sale_id AND s.deleted_at IS NULL
		JOIN payments p ON p.id = s.payment_id AND p.deleted_at IS NULL
		WHERE si.gym_id = ? AND si.deleted_at IS NULL
		  AND p.concept = 'product'
		  AND p.payment_date >= ? AND p.payment_date <= ?`,
		gymID, from.Format(dateFmt), to.Format(dateFmt)).Scan(&out.Units).Error; err != nil {
		return reports.ProductSalesTotals{}, err
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
		    COALESCE(COUNT(*) FILTER (WHERE stock <= 0), 0) AS out_count,
		    COALESCE(COUNT(*) FILTER (WHERE stock > 0 AND stock <= stock_minimum), 0) AS low_count
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
		WHERE m.gym_id = ?
		  AND m.status = 'active'
		  AND m.deleted_at IS NULL
		  AND EXISTS (
		      SELECT 1 FROM memberships ms
		      WHERE ms.member_id = m.id AND ms.deleted_at IS NULL
		        AND ms.status IN ('active', 'replaced')
		        AND ms.start_date <= ? AND ms.expiry_date >= ?
		  )`,
		gymID, today.Format(dateFmt), today.Format(dateFmt)).Scan(&row).Error
	return reports.GenderCompositionRow{
		Hombre: row.Hombre, Mujer: row.Mujer,
		NoEspecificado: row.NoEspecificado, Total: row.Total,
	}, err
}

// AttendanceByGenderHour — heatmap de check-ins exitosos × hora × género.
// "Exitoso" = result LIKE 'allowed_%' (allowed_active / expiring_soon /
// override). Filtramos denied_* para que el reporte refleje uso real del gym,
// no intentos fallidos. La ventana corre [now - daysBack, now] sobre
// checkin_at y la hora se clasifica en la zona LOCAL del gym — con la hora
// UTC el heatmap salía corrido 6 horas en CDMX.
func (r *PostgresReader) AttendanceByGenderHour(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, daysBack int, now time.Time) ([]reports.AttendanceByGenderHourRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	cutoff := now.Add(-time.Duration(daysBack) * 24 * time.Hour)
	zone := tz.NameOrUTC(tzName)
	var rows []hourBucketRow
	err := gormTx.Raw(`
		SELECT
		    EXTRACT(HOUR FROM (c.checkin_at AT TIME ZONE ?))::int                    AS hour,
		    COUNT(*) FILTER (WHERE m.gender = 'hombre')                              AS hombre,
		    COUNT(*) FILTER (WHERE m.gender = 'mujer')                               AS mujer,
		    COUNT(*) FILTER (WHERE m.gender IS NULL OR m.gender = 'no_especificado') AS no_especificado
		FROM checkins c
		JOIN members m ON m.id = c.member_id AND m.deleted_at IS NULL
		WHERE c.gym_id = ?
		  AND c.deleted_at IS NULL
		  AND c.result LIKE 'allowed_%'
		  AND c.checkin_at >= ? AND c.checkin_at <= ?
		GROUP BY 1`,
		zone, gymID, cutoff, now).Scan(&rows).Error
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
