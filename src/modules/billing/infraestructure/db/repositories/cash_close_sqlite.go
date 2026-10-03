//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/mattn/go-sqlite3"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CashCloseSQLiteReader struct{}

func NewCashCloseSQLiteReader() *CashCloseSQLiteReader { return &CashCloseSQLiteReader{} }

// Aggregate computes the per-day rollup. Money is stored in cents (SQLite),
// so we convert at the edge. Mirrors the Postgres implementation contract,
// including concept counts + operator names + sales count.
func (r *CashCloseSQLiteReader) Aggregate(tx sharedDomain.Transaction, q billingRepo.CashCloseQuery) (*billingRepo.CashCloseTotals, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	dateStr := q.Date.UTC().Format("2006-01-02")
	drawerID := q.DrawerID
	if drawerID == uuid.Nil {
		drawerID = q.GymID
	}

	type methodRow struct {
		PaymentMethod string `db:"payment_method"`
		Total         int64  `db:"total"`
	}
	var methodRows []methodRow
	if err := stx.Select(context.Background(), &methodRows,
		`SELECT payment_method, COALESCE(SUM(amount), 0) AS total FROM payments
		 WHERE amount<>0 AND gym_id = ? AND payment_date = ? AND concept <> ? AND deleted_at IS NULL
		   AND (payment_method<>'cash' OR (cash_destination<>'gym_fund' AND COALESCE(cash_drawer_id,gym_id)=?))
		 GROUP BY payment_method`,
		q.GymID.String(), dateStr, paymentDomain.ConceptRefund, drawerID.String()); err != nil {
		return nil, err
	}

	type conceptRow struct {
		Concept string `db:"concept"`
		Total   int64  `db:"total"`
		Cnt     int    `db:"cnt"`
	}
	var conceptRows []conceptRow
	if err := stx.Select(context.Background(), &conceptRows,
		`SELECT concept, COALESCE(SUM(amount), 0) AS total, COUNT(*) AS cnt FROM payments
		 WHERE amount<>0 AND gym_id = ? AND payment_date = ? AND concept <> ? AND deleted_at IS NULL
		   AND (payment_method<>'cash' OR (cash_destination<>'gym_fund' AND COALESCE(cash_drawer_id,gym_id)=?))
		 GROUP BY concept`,
		q.GymID.String(), dateStr, paymentDomain.ConceptRefund, drawerID.String()); err != nil {
		return nil, err
	}

	// Refunds agrupados por método: el total alimenta RefundTotal y cada
	// método RefundByMethod (para descontar del cajón los refunds en efectivo,
	// que sí sacan dinero). amount es negativo → la suma es negativa.
	type refundRow struct {
		PaymentMethod string `db:"payment_method"`
		Total         int64  `db:"total"`
		Cnt           int    `db:"cnt"`
	}
	var refundRows []refundRow
	if err := stx.Select(context.Background(), &refundRows,
		`SELECT payment_method, COALESCE(SUM(amount), 0) AS total, COUNT(*) AS cnt FROM payments
		 WHERE amount<>0 AND gym_id = ? AND payment_date = ? AND concept = ? AND deleted_at IS NULL
		   AND (payment_method<>'cash' OR (cash_destination<>'gym_fund' AND COALESCE(cash_drawer_id,gym_id)=?))
		 GROUP BY payment_method`,
		q.GymID.String(), dateStr, paymentDomain.ConceptRefund, drawerID.String()); err != nil {
		return nil, err
	}

	type opRow struct {
		OperatorID   string         `db:"operator_id"`
		OperatorName sql.NullString `db:"operator_name"`
		Total        int64          `db:"total"`
		PaymentsN    int            `db:"payments_n"`
		SalesN       int            `db:"sales_n"`
	}
	var opRows []opRow
	if err := stx.Select(context.Background(), &opRows,
		`SELECT p.operator_id,
		        u.full_name AS operator_name,
		        COALESCE(SUM(p.amount), 0) AS total,
		        COUNT(*) AS payments_n,
		        SUM(CASE WHEN p.concept = 'product' THEN 1 ELSE 0 END) AS sales_n
		 FROM payments p
		 LEFT JOIN users u ON u.id = p.operator_id AND u.deleted_at IS NULL
		 WHERE p.amount<>0 AND p.gym_id = ? AND p.payment_date = ?
		   AND p.concept <> ? AND p.deleted_at IS NULL
		   AND (p.payment_method<>'cash' OR (p.cash_destination<>'gym_fund' AND COALESCE(p.cash_drawer_id,p.gym_id)=?))
		 GROUP BY p.operator_id, u.full_name
		 ORDER BY total DESC`,
		q.GymID.String(), dateStr, paymentDomain.ConceptRefund, drawerID.String()); err != nil {
		return nil, err
	}

	out := &billingRepo.CashCloseTotals{
		ByMethod:       map[string]float64{},
		ByConcept:      map[string]billingRepo.ConceptTotal{},
		ByOperator:     make([]billingRepo.OperatorTotal, 0, len(opRows)),
		RefundByMethod: map[string]float64{},
	}
	for _, rf := range refundRows {
		v := fromCents(rf.Total) // negativo
		out.RefundByMethod[rf.PaymentMethod] = v
		out.RefundTotal += v
		out.RefundCount += rf.Cnt
	}
	for _, m := range methodRows {
		v := fromCents(m.Total)
		out.ByMethod[m.PaymentMethod] = v
		out.GrandTotal += v
	}
	for _, c := range conceptRows {
		out.ByConcept[c.Concept] = billingRepo.ConceptTotal{
			Total: fromCents(c.Total),
			Count: c.Cnt,
		}
	}
	for _, o := range opRows {
		opID, _ := uuid.Parse(o.OperatorID)
		name := ""
		if o.OperatorName.Valid {
			name = o.OperatorName.String
		}
		out.ByOperator = append(out.ByOperator, billingRepo.OperatorTotal{
			OperatorID:   opID,
			OperatorName: name,
			Total:        fromCents(o.Total),
			PaymentsN:    o.PaymentsN,
			SalesN:       o.SalesN,
		})
	}
	return out, nil
}

func (r *CashCloseSQLiteReader) HasCashActivityAfter(tx sharedDomain.Transaction, q billingRepo.CashCloseQuery, after time.Time) (bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	drawerID := q.DrawerID
	if drawerID == uuid.Nil {
		drawerID = q.GymID
	}
	var found int
	err := stx.Get(context.Background(), &found, `
		SELECT EXISTS (
			SELECT 1 FROM payments
			WHERE amount<>0 AND gym_id = ? AND payment_date = ? AND payment_method = 'cash' AND cash_destination<>'gym_fund'
			  AND COALESCE(cash_drawer_id,gym_id)=?
			  AND deleted_at IS NULL AND (created_at > ? OR updated_at > ?)
			UNION ALL
			SELECT 1 FROM cash_movements
			WHERE gym_id = ? AND movement_on = ? AND deleted_at IS NULL
			  AND COALESCE(cash_drawer_id,gym_id)=?
			  AND (created_at > ? OR updated_at > ?)
			LIMIT 1
		)`,
		q.GymID.String(), q.Date.UTC().Format("2006-01-02"), drawerID.String(), after.UnixMilli(), after.UnixMilli(),
		q.GymID.String(), q.Date.UTC().Format("2006-01-02"), drawerID.String(), after.UnixMilli(), after.UnixMilli())
	return found != 0, err
}

func (r *CashCloseSQLiteReader) SessionActivity(tx sharedDomain.Transaction, q billingRepo.CashSessionActivityQuery) (*billingRepo.CashSessionActivity, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if q.DrawerID == uuid.Nil {
		q.DrawerID = q.GymID
	}
	fromMs := q.OpenedAt.UTC().UnixMilli()
	toMs := q.AsOf.UTC().UnixMilli()
	if q.AsOf.IsZero() {
		toMs = time.Now().UTC().UnixMilli()
	}
	dateStr := q.OperationalDate.UTC().Format("2006-01-02")
	type activityRow struct {
		Payments  int64         `db:"payments"`
		CashIn    int64         `db:"cash_in"`
		CashOut   int64         `db:"cash_out"`
		Watermark sql.NullInt64 `db:"watermark"`
	}
	var row activityRow
	err := stx.Get(context.Background(), &row, `
		WITH physical AS (
			SELECT amount AS payment_amount, 0 AS cash_in, 0 AS cash_out,
			       created_at AS touched_at
			  FROM payments
			 WHERE amount<>0 AND gym_id=? AND payment_date=? AND payment_method='cash' AND cash_destination<>'gym_fund'
			   AND COALESCE(cash_drawer_id,gym_id)=? AND deleted_at IS NULL
			   AND created_at>=? AND created_at<=?
			UNION ALL
			SELECT 0,
			       CASE WHEN movement_type='cash_in' THEN amount ELSE 0 END,
			       CASE WHEN movement_type='cash_out' THEN amount ELSE 0 END,
			       created_at
			  FROM cash_movements
			 WHERE gym_id=? AND movement_on=? AND COALESCE(cash_drawer_id,gym_id)=?
			   AND deleted_at IS NULL AND created_at>=? AND created_at<=?
		)
		SELECT COALESCE(SUM(payment_amount),0) AS payments,
		       COALESCE(SUM(cash_in),0) AS cash_in,
		       COALESCE(SUM(cash_out),0) AS cash_out,
		       MAX(touched_at) AS watermark
		  FROM physical`,
		q.GymID.String(), dateStr, q.DrawerID.String(), fromMs, toMs,
		q.GymID.String(), dateStr, q.DrawerID.String(), fromMs, toMs)
	if err != nil {
		return nil, err
	}
	out := &billingRepo.CashSessionActivity{
		PaymentsCash: fromCents(row.Payments),
		CashIn:       fromCents(row.CashIn),
		CashOut:      fromCents(row.CashOut),
	}
	out.Net = out.PaymentsCash + out.CashIn - out.CashOut
	if row.Watermark.Valid {
		out.Watermark = time.UnixMilli(row.Watermark.Int64).UTC()
	}
	return out, nil
}

func (r *CashCloseSQLiteReader) CashDrawers(tx sharedDomain.Transaction, gymID uuid.UUID, date time.Time) ([]billingRepo.CashDrawerActivity, error) {
	type drawerRow struct {
		DrawerID       string `db:"drawer_id"`
		ActivityCash   int64  `db:"activity_cash"`
		LastActivityAt int64  `db:"last_activity_at"`
	}
	var rows []drawerRow
	err := tx.(*sharedDomain.SqlxTransaction).Select(context.Background(), &rows, `
		WITH physical(drawer_id,amount,touched_at) AS (
			SELECT COALESCE(cash_drawer_id,gym_id),amount,created_at
			  FROM payments
			 WHERE amount<>0 AND gym_id=? AND payment_date=? AND payment_method='cash' AND cash_destination<>'gym_fund' AND deleted_at IS NULL
			UNION ALL
			SELECT COALESCE(cash_drawer_id,gym_id),
			       CASE WHEN movement_type='cash_in' THEN amount ELSE -amount END,
			       created_at
			  FROM cash_movements
			 WHERE gym_id=? AND movement_on=? AND deleted_at IS NULL
		)
		SELECT drawer_id,SUM(amount) AS activity_cash,MAX(touched_at) AS last_activity_at
		  FROM physical GROUP BY drawer_id ORDER BY drawer_id`,
		gymID.String(), date.UTC().Format("2006-01-02"),
		gymID.String(), date.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	out := make([]billingRepo.CashDrawerActivity, 0, len(rows))
	for _, row := range rows {
		drawerID, parseErr := uuid.Parse(row.DrawerID)
		if parseErr != nil {
			return nil, parseErr
		}
		out = append(out, billingRepo.CashDrawerActivity{
			DrawerID: drawerID, ActivityCash: fromCents(row.ActivityCash),
			LastActivityAt: time.UnixMilli(row.LastActivityAt).UTC(),
		})
	}
	return out, nil
}

func (r *CashCloseSQLiteReader) SessionCoverage(tx sharedDomain.Transaction, q billingRepo.CashSessionCoverageQuery) (*billingRepo.CashSessionCoverage, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	from, to := q.From.UTC().Format("2006-01-02"), q.To.UTC().Format("2006-01-02")
	type coverageRow struct {
		ActiveDays               int `db:"active_days"`
		ActiveSessions           int `db:"active_sessions"`
		MissingDays              int `db:"missing_days"`
		UncoveredDays            int `db:"uncovered_days"`
		OpenN                    int `db:"open_n"`
		UnverifiedN              int `db:"unverified_n"`
		StaleN                   int `db:"stale_n"`
		ReconciledN              int `db:"reconciled_n"`
		WithdrawnN               int `db:"withdrawn_n"`
		UnknownOpeningN          int `db:"unknown_opening_n"`
		AdjustedAfterWithdrawalN int `db:"adjusted_after_withdrawal_n"`
	}
	var row coverageRow
	err := stx.Get(context.Background(), &row, `
		WITH physical(day,drawer_id,recorded_at,touched_at,amount) AS (
			SELECT payment_date,COALESCE(cash_drawer_id,gym_id),created_at,created_at,amount
			  FROM payments
			 WHERE amount<>0 AND gym_id=? AND payment_method='cash' AND cash_destination<>'gym_fund' AND deleted_at IS NULL
			   AND payment_date BETWEEN ? AND ?
			UNION ALL
			SELECT movement_on,COALESCE(cash_drawer_id,gym_id),created_at,created_at,
			       CASE WHEN movement_type='cash_in' THEN amount ELSE -amount END
			  FROM cash_movements
			 WHERE gym_id=? AND deleted_at IS NULL AND movement_on BETWEEN ? AND ?
		), active_dates(day) AS (
			SELECT DISTINCT day FROM physical
		), sessions AS (
			SELECT * FROM cash_close_events
			 WHERE gym_id=? AND operational_date BETWEEN ? AND ? AND deleted_at IS NULL
		), current_activity AS (
			SELECT s.id,COALESCE(SUM(p.amount),0) AS amount
			  FROM sessions s
			  LEFT JOIN physical p ON p.day=s.operational_date AND p.drawer_id=s.drawer_id
			   AND p.recorded_at>=s.opened_at
			   AND (COALESCE(s.withdrawn_at, CASE WHEN s.cash_left IS NOT NULL THEN s.reconciled_at END) IS NULL OR p.recorded_at<=COALESCE(s.withdrawn_at, CASE WHEN s.cash_left IS NOT NULL THEN s.reconciled_at END))
			 GROUP BY s.id
		), uncovered_dates(day) AS (
			SELECT DISTINCT p.day FROM physical p
			WHERE (SELECT COUNT(*) FROM sessions s
			       WHERE s.operational_date=p.day AND s.drawer_id=p.drawer_id
			         AND p.recorded_at>=s.opened_at
			         AND (s.status<>'withdrawn' OR COALESCE(s.withdrawn_at, CASE WHEN s.cash_left IS NOT NULL THEN s.reconciled_at END) IS NOT NULL)
			         AND (COALESCE(s.withdrawn_at, CASE WHEN s.cash_left IS NOT NULL THEN s.reconciled_at END) IS NULL OR p.recorded_at<=COALESCE(s.withdrawn_at, CASE WHEN s.cash_left IS NOT NULL THEN s.reconciled_at END))) <> 1
			   OR EXISTS (SELECT 1 FROM sessions s
			       WHERE s.operational_date=p.day AND s.drawer_id=p.drawer_id
			         AND p.recorded_at>=s.opened_at AND s.closed_at IS NOT NULL
			         AND p.touched_at>s.closed_at
			         AND ((s.status<>'withdrawn' AND s.cash_left IS NULL)
			           OR (COALESCE(s.withdrawn_at, CASE WHEN s.cash_left IS NOT NULL THEN s.reconciled_at END) IS NOT NULL AND p.touched_at<=COALESCE(s.withdrawn_at, CASE WHEN s.cash_left IS NOT NULL THEN s.reconciled_at END))))
			UNION
			SELECT s.operational_date FROM sessions s
			JOIN current_activity ca ON ca.id=s.id
			WHERE s.status<>'open' AND ca.amount<>COALESCE(s.activity_cash,0)
		), session_flags AS (
			SELECT s.*,
			       CASE WHEN s.status='stale' THEN 1
			         WHEN s.status IN ('open','withdrawn') THEN 0
			         WHEN ca.amount<>COALESCE(s.activity_cash,0) THEN 1
			         WHEN s.closed_at IS NOT NULL AND EXISTS (
			             SELECT 1 FROM physical p
			              WHERE p.day=s.operational_date AND p.drawer_id=s.drawer_id
			                AND p.recorded_at>=s.opened_at AND p.recorded_at>s.closed_at
			                AND (COALESCE(s.withdrawn_at, CASE WHEN s.cash_left IS NOT NULL THEN s.reconciled_at END) IS NULL OR p.recorded_at<=COALESCE(s.withdrawn_at, CASE WHEN s.cash_left IS NOT NULL THEN s.reconciled_at END))
			         ) THEN 1 ELSE 0 END AS derived_stale,
			       CASE WHEN s.adjusted_after_withdrawal=1 OR (s.status='withdrawn'
			          AND ca.amount<>COALESCE(s.activity_cash,0)) THEN 1 ELSE 0 END AS derived_adjusted
			  FROM sessions s JOIN current_activity ca ON ca.id=s.id
		)
		SELECT (SELECT COUNT(*) FROM active_dates) AS active_days,
		       (SELECT COUNT(*) FROM sessions) AS active_sessions,
		       (SELECT COUNT(*) FROM active_dates a WHERE NOT EXISTS
		          (SELECT 1 FROM sessions s WHERE s.operational_date=a.day)) AS missing_days,
		       (SELECT COUNT(*) FROM uncovered_dates) AS uncovered_days,
		       (SELECT COUNT(*) FROM session_flags WHERE status='open') AS open_n,
		       (SELECT COUNT(*) FROM session_flags
		         WHERE status='closed_unverified' AND derived_stale=0) AS unverified_n,
		       (SELECT COUNT(*) FROM session_flags WHERE derived_stale=1) AS stale_n,
		       (SELECT COUNT(*) FROM session_flags
		         WHERE status='reconciled' AND derived_stale=0) AS reconciled_n,
		       (SELECT COUNT(*) FROM session_flags WHERE status='withdrawn') AS withdrawn_n,
		       (SELECT COUNT(*) FROM session_flags WHERE opening_cash_known=0) AS unknown_opening_n,
		       (SELECT COUNT(*) FROM session_flags WHERE derived_adjusted=1) AS adjusted_after_withdrawal_n`,
		q.GymID.String(), from, to, q.GymID.String(), from, to,
		q.GymID.String(), from, to)
	if err != nil {
		return nil, err
	}
	out := &billingRepo.CashSessionCoverage{
		ActiveDays: row.ActiveDays, ActiveSessions: row.ActiveSessions,
		MissingActiveDays: row.MissingDays, UncoveredActivityDays: row.UncoveredDays,
		OpenSessions:             row.OpenN,
		ClosedUnverifiedSessions: row.UnverifiedN, StaleSessions: row.StaleN,
		ReconciledSessions: row.ReconciledN, WithdrawnSessions: row.WithdrawnN,
		UnknownOpeningSessions:          row.UnknownOpeningN,
		AdjustedAfterWithdrawalSessions: row.AdjustedAfterWithdrawalN,
	}
	out.Complete = out.MissingActiveDays == 0 && out.UncoveredActivityDays == 0 && out.OpenSessions == 0 &&
		out.ClosedUnverifiedSessions == 0 && out.StaleSessions == 0 &&
		out.UnknownOpeningSessions == 0 && out.AdjustedAfterWithdrawalSessions == 0
	return out, nil
}

type CashCloseEventSQLiteRepository struct{}

func NewCashCloseEventSQLiteRepository() *CashCloseEventSQLiteRepository {
	return &CashCloseEventSQLiteRepository{}
}

type sqliteCashCloseEventRow struct {
	ID                      string         `db:"id"`
	GymID                   string         `db:"gym_id"`
	Version                 int            `db:"version"`
	CreatedAt               int64          `db:"created_at"`
	UpdatedAt               int64          `db:"updated_at"`
	DeletedAt               sql.NullInt64  `db:"deleted_at"`
	SyncedAt                sql.NullInt64  `db:"synced_at"`
	DrawerID                string         `db:"drawer_id"`
	DrawerCode              string         `db:"drawer_code"`
	OperationalDate         string         `db:"operational_date"`
	Sequence                int            `db:"sequence"`
	Status                  string         `db:"status"`
	CloseDate               string         `db:"close_date"`
	OpeningCash             int64          `db:"opening_cash"`
	OpeningCashKnown        int            `db:"opening_cash_known"`
	ActivityCash            int64          `db:"activity_cash"`
	CalculatedCash          int64          `db:"calculated_cash"`
	CountedCash             sql.NullInt64  `db:"counted_cash"`
	CashLeft                sql.NullInt64  `db:"cash_left"`
	WithdrawnCash           sql.NullInt64  `db:"withdrawn_cash"`
	WithdrawalDestination   sql.NullString `db:"withdrawal_destination"`
	Discrepancy             sql.NullInt64  `db:"discrepancy"`
	DiscrepancyReason       sql.NullString `db:"discrepancy_reason"`
	CorrectionReason        sql.NullString `db:"correction_reason"`
	AdjustedAfterWithdrawal int            `db:"adjusted_after_withdrawal"`
	IntegrityNote           sql.NullString `db:"integrity_note"`
	OpenedAt                int64          `db:"opened_at"`
	OpenedBy                string         `db:"opened_by"`
	ClosedAt                sql.NullInt64  `db:"closed_at"`
	ClosedBy                string         `db:"closed_by"`
	ReconciledAt            sql.NullInt64  `db:"reconciled_at"`
	ReconciledBy            sql.NullString `db:"reconciled_by"`
	StaleAt                 sql.NullInt64  `db:"stale_at"`
	WithdrawnAt             sql.NullInt64  `db:"withdrawn_at"`
	WithdrawnBy             sql.NullString `db:"withdrawn_by"`
}

func (r *CashCloseEventSQLiteRepository) Create(tx sharedDomain.Transaction, e *cashCloseDomain.CashCloseEvent) (*cashCloseDomain.CashCloseEvent, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := cashCloseEventToRow(e)
	const stmt = `
		INSERT INTO cash_close_events (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    drawer_id, drawer_code, operational_date, sequence, status,
		    close_date, opening_cash, opening_cash_known, activity_cash,
		    calculated_cash, counted_cash, cash_left, withdrawn_cash,
		    withdrawal_destination, discrepancy_reason, correction_reason,
		    adjusted_after_withdrawal, integrity_note,
		    opened_at, opened_by, closed_at, closed_by, reconciled_at,
		    reconciled_by, stale_at, withdrawn_at, withdrawn_by
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :drawer_id, :drawer_code, :operational_date, :sequence, :status,
		    :close_date, :opening_cash, :opening_cash_known, :activity_cash,
		    :calculated_cash, :counted_cash, :cash_left, :withdrawn_cash,
		    :withdrawal_destination, :discrepancy_reason, :correction_reason,
		    :adjusted_after_withdrawal, :integrity_note,
		    :opened_at, :opened_by, :closed_at, :closed_by, :reconciled_at,
		    :reconciled_by, :stale_at, :withdrawn_at, :withdrawn_by
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		var sqliteErr sqlite3.Error
		if errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique {
			return nil, billingErrors.ErrCashCloseAlreadyExists
		}
		return nil, err
	}
	if err := enqueueCashCloseEvent(stx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (r *CashCloseEventSQLiteRepository) Update(tx sharedDomain.Transaction, e *cashCloseDomain.CashCloseEvent) (*cashCloseDomain.CashCloseEvent, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := cashCloseEventToRow(e)
	res, err := stx.NamedExec(context.Background(), `
		UPDATE cash_close_events SET
			version=:version, updated_at=:updated_at, deleted_at=:deleted_at,
			drawer_id=:drawer_id, drawer_code=:drawer_code,
			operational_date=:operational_date, sequence=:sequence, status=:status,
			close_date=:close_date, opening_cash=:opening_cash,
			opening_cash_known=:opening_cash_known, activity_cash=:activity_cash,
			calculated_cash=:calculated_cash, counted_cash=:counted_cash,
			cash_left=:cash_left, withdrawn_cash=:withdrawn_cash,
			withdrawal_destination=:withdrawal_destination,
			discrepancy_reason=:discrepancy_reason, correction_reason=:correction_reason,
			adjusted_after_withdrawal=:adjusted_after_withdrawal, integrity_note=:integrity_note,
			opened_at=:opened_at, opened_by=:opened_by, closed_at=:closed_at,
			closed_by=:closed_by, reconciled_at=:reconciled_at,
			reconciled_by=:reconciled_by, stale_at=:stale_at,
			withdrawn_at=:withdrawn_at, withdrawn_by=:withdrawn_by
		WHERE gym_id=:gym_id AND id=:id AND version=:previous_version`, map[string]any{
		"id": row.ID, "gym_id": row.GymID, "version": row.Version,
		"previous_version": row.Version - 1, "updated_at": row.UpdatedAt,
		"deleted_at": nullCashCloseInt(row.DeletedAt), "calculated_cash": row.CalculatedCash,
		"counted_cash": nullCashCloseInt(row.CountedCash),
		"drawer_id":    row.DrawerID, "drawer_code": row.DrawerCode,
		"operational_date": row.OperationalDate, "sequence": row.Sequence, "status": row.Status,
		"close_date": row.CloseDate, "opening_cash": row.OpeningCash,
		"opening_cash_known": row.OpeningCashKnown, "activity_cash": row.ActivityCash,
		"cash_left": nullCashCloseInt(row.CashLeft), "withdrawn_cash": nullCashCloseInt(row.WithdrawnCash),
		"withdrawal_destination":    nullCashCloseString(row.WithdrawalDestination),
		"discrepancy_reason":        nullCashCloseString(row.DiscrepancyReason),
		"correction_reason":         nullCashCloseString(row.CorrectionReason),
		"adjusted_after_withdrawal": row.AdjustedAfterWithdrawal,
		"integrity_note":            nullCashCloseString(row.IntegrityNote),
		"opened_at":                 row.OpenedAt, "opened_by": row.OpenedBy,
		"closed_at": nullCashCloseInt(row.ClosedAt), "closed_by": row.ClosedBy,
		"reconciled_at": nullCashCloseInt(row.ReconciledAt),
		"reconciled_by": nullCashCloseString(row.ReconciledBy),
		"stale_at":      nullCashCloseInt(row.StaleAt), "withdrawn_at": nullCashCloseInt(row.WithdrawnAt),
		"withdrawn_by": nullCashCloseString(row.WithdrawnBy),
	})
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, billingErrors.ErrCashCloseConflict
	}
	if err := enqueueCashCloseEvent(stx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (r *CashCloseEventSQLiteRepository) GetByID(tx sharedDomain.Transaction, gymID, id uuid.UUID) (*cashCloseDomain.CashCloseEvent, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteCashCloseEventRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM cash_close_events WHERE gym_id=? AND id=? AND deleted_at IS NULL`, gymID.String(), id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cashCloseEventFromRow(&row), nil
}

func (r *CashCloseEventSQLiteRepository) GetByNaturalKey(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID, date time.Time, sequence int) (*cashCloseDomain.CashCloseEvent, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteCashCloseEventRow
	err := stx.Get(context.Background(), &row, `SELECT * FROM cash_close_events
		WHERE gym_id=? AND drawer_id=? AND operational_date=? AND sequence=? AND deleted_at IS NULL`,
		gymID.String(), drawerID.String(), date.UTC().Format("2006-01-02"), sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cashCloseEventFromRow(&row), nil
}

func (r *CashCloseEventSQLiteRepository) GetByDate(tx sharedDomain.Transaction, gymID uuid.UUID, date time.Time) (*cashCloseDomain.CashCloseEvent, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteCashCloseEventRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM cash_close_events
		 WHERE gym_id = ? AND drawer_id = ? AND operational_date = ? AND deleted_at IS NULL
		 ORDER BY sequence DESC LIMIT 1`,
		gymID.String(), cashCloseDomain.DefaultDrawerID(gymID).String(), date.Format("2006-01-02"))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cashCloseEventFromRow(&row), nil
}

func (r *CashCloseEventSQLiteRepository) ListByDate(tx sharedDomain.Transaction, gymID uuid.UUID, date time.Time) ([]*cashCloseDomain.CashCloseEvent, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteCashCloseEventRow
	if err := stx.Select(context.Background(), &rows, `SELECT * FROM cash_close_events
		WHERE gym_id=? AND operational_date=? AND deleted_at IS NULL ORDER BY drawer_code,sequence`,
		gymID.String(), date.UTC().Format("2006-01-02")); err != nil {
		return nil, err
	}
	out := make([]*cashCloseDomain.CashCloseEvent, len(rows))
	for i := range rows {
		out[i] = cashCloseEventFromRow(&rows[i])
	}
	return out, nil
}

func (r *CashCloseEventSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]*cashCloseDomain.CashCloseEvent, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var rows []sqliteCashCloseEventRow
	if err := stx.Select(context.Background(), &rows,
		`SELECT * FROM cash_close_events WHERE gym_id = ? AND deleted_at IS NULL
		 ORDER BY operational_date DESC, sequence DESC LIMIT ?`, gymID.String(), limit); err != nil {
		return nil, err
	}
	out := make([]*cashCloseDomain.CashCloseEvent, len(rows))
	for i := range rows {
		out[i] = cashCloseEventFromRow(&rows[i])
	}
	return out, nil
}

func cashCloseEventToRow(e *cashCloseDomain.CashCloseEvent) sqliteCashCloseEventRow {
	row := sqliteCashCloseEventRow{
		ID: e.ID.String(), GymID: e.GymID.String(), Version: e.Version,
		CreatedAt: e.CreatedAt.UnixMilli(), UpdatedAt: e.UpdatedAt.UnixMilli(),
		DrawerID: e.DrawerID.String(), DrawerCode: e.DrawerCode,
		OperationalDate: e.OperationalDate.UTC().Format("2006-01-02"),
		Sequence:        e.Sequence, Status: e.Status,
		CloseDate:   e.CloseDate.UTC().Format("2006-01-02"),
		OpeningCash: toCents(e.OpeningCash), ActivityCash: toCents(e.ActivityCash),
		CalculatedCash: toCents(e.CalculatedCash),
		OpenedAt:       e.OpenedAt.UnixMilli(), OpenedBy: e.OpenedBy.String(),
		ClosedBy: e.ClosedBy.String(),
	}
	if e.OpeningCashKnown {
		row.OpeningCashKnown = 1
	}
	if e.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: e.DeletedAt.UnixMilli(), Valid: true}
	}
	if e.CountedCash != nil {
		row.CountedCash = sql.NullInt64{Int64: toCents(*e.CountedCash), Valid: true}
	}
	if e.CashLeft != nil {
		row.CashLeft = sql.NullInt64{Int64: toCents(*e.CashLeft), Valid: true}
	}
	if e.WithdrawnCash != nil {
		row.WithdrawnCash = sql.NullInt64{Int64: toCents(*e.WithdrawnCash), Valid: true}
	}
	row.WithdrawalDestination = cashCloseNullString(e.WithdrawalDestination)
	if e.DiscrepancyReason != nil {
		row.DiscrepancyReason = sql.NullString{String: *e.DiscrepancyReason, Valid: true}
	}
	row.CorrectionReason = cashCloseNullString(e.CorrectionReason)
	if e.AdjustedAfterWithdrawal {
		row.AdjustedAfterWithdrawal = 1
	}
	row.IntegrityNote = cashCloseNullString(e.IntegrityNote)
	row.ClosedAt = cashCloseNullTime(e.ClosedAt)
	row.ReconciledAt = cashCloseNullTime(e.ReconciledAt)
	row.ReconciledBy = cashCloseNullUUID(e.ReconciledBy)
	row.StaleAt = cashCloseNullTime(e.StaleAt)
	row.WithdrawnAt = cashCloseNullTime(e.WithdrawnAt)
	row.WithdrawnBy = cashCloseNullUUID(e.WithdrawnBy)
	return row
}

func cashCloseEventFromRow(r *sqliteCashCloseEventRow) *cashCloseDomain.CashCloseEvent {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	closedBy, _ := uuid.Parse(r.ClosedBy)
	drawerID, _ := uuid.Parse(r.DrawerID)
	openedBy, _ := uuid.Parse(r.OpenedBy)
	closeDate, _ := time.Parse("2006-01-02", r.CloseDate)
	operationalDate, _ := time.Parse("2006-01-02", r.OperationalDate)
	e := &cashCloseDomain.CashCloseEvent{
		ID: id, GymID: gymID, Version: r.Version,
		DrawerID: drawerID, DrawerCode: r.DrawerCode,
		OperationalDate: operationalDate, Sequence: r.Sequence, Status: r.Status,
		CloseDate: closeDate, OpeningCash: fromCents(r.OpeningCash),
		OpeningCashKnown: r.OpeningCashKnown != 0, ActivityCash: fromCents(r.ActivityCash),
		CalculatedCash: fromCents(r.CalculatedCash),
		OpenedAt:       time.UnixMilli(r.OpenedAt).UTC(), OpenedBy: openedBy,
		ClosedBy: closedBy, CreatedAt: time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt: time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		e.DeletedAt = &t
	}
	if r.CountedCash.Valid {
		v := fromCents(r.CountedCash.Int64)
		e.CountedCash = &v
	}
	if r.CashLeft.Valid {
		v := fromCents(r.CashLeft.Int64)
		e.CashLeft = &v
	}
	if r.WithdrawnCash.Valid {
		v := fromCents(r.WithdrawnCash.Int64)
		e.WithdrawnCash = &v
	}
	e.WithdrawalDestination = cashCloseStringPtr(r.WithdrawalDestination)
	if r.DiscrepancyReason.Valid {
		s := r.DiscrepancyReason.String
		e.DiscrepancyReason = &s
	}
	e.CorrectionReason = cashCloseStringPtr(r.CorrectionReason)
	e.AdjustedAfterWithdrawal = r.AdjustedAfterWithdrawal != 0
	e.IntegrityNote = cashCloseStringPtr(r.IntegrityNote)
	e.ClosedAt = cashCloseTimePtr(r.ClosedAt)
	e.ReconciledAt = cashCloseTimePtr(r.ReconciledAt)
	e.ReconciledBy = cashCloseUUIDPtr(r.ReconciledBy)
	e.StaleAt = cashCloseTimePtr(r.StaleAt)
	e.WithdrawnAt = cashCloseTimePtr(r.WithdrawnAt)
	e.WithdrawnBy = cashCloseUUIDPtr(r.WithdrawnBy)
	return e
}

func enqueueCashCloseEvent(stx *sharedDomain.SqlxTransaction, e *cashCloseDomain.CashCloseEvent) error {
	if stx.Queue == nil {
		return nil
	}
	var counted any
	if e.CountedCash != nil {
		counted = *e.CountedCash
	}
	var reason any
	if e.DiscrepancyReason != nil {
		reason = *e.DiscrepancyReason
	}
	payload, err := json.Marshal(map[string]any{
		"id":                        e.ID.String(),
		"gym_id":                    e.GymID.String(),
		"version":                   e.Version,
		"close_date":                e.CloseDate.UTC().Format("2006-01-02"),
		"calculated_cash":           e.CalculatedCash,
		"counted_cash":              counted,
		"discrepancy_reason":        reason,
		"closed_by":                 e.ClosedBy.String(),
		"created_at":                e.CreatedAt.UnixMilli(),
		"updated_at":                e.UpdatedAt.UnixMilli(),
		"deleted_at":                cashCloseMillis(e.DeletedAt),
		"drawer_id":                 e.DrawerID.String(),
		"drawer_code":               e.DrawerCode,
		"operational_date":          e.OperationalDate.UTC().Format("2006-01-02"),
		"sequence":                  e.Sequence,
		"status":                    e.Status,
		"opening_cash":              e.OpeningCash,
		"opening_cash_known":        e.OpeningCashKnown,
		"activity_cash":             e.ActivityCash,
		"cash_left":                 e.CashLeft,
		"withdrawn_cash":            e.WithdrawnCash,
		"withdrawal_destination":    e.WithdrawalDestination,
		"correction_reason":         e.CorrectionReason,
		"adjusted_after_withdrawal": e.AdjustedAfterWithdrawal,
		"integrity_note":            e.IntegrityNote,
		"opened_at":                 e.OpenedAt.UnixMilli(),
		"opened_by":                 e.OpenedBy.String(),
		"closed_at":                 cashCloseMillis(e.ClosedAt),
		"reconciled_at":             cashCloseMillis(e.ReconciledAt),
		"reconciled_by":             e.ReconciledBy,
		"stale_at":                  cashCloseMillis(e.StaleAt),
		"withdrawn_at":              cashCloseMillis(e.WithdrawnAt),
		"withdrawn_by":              e.WithdrawnBy,
	})
	if err != nil {
		return err
	}
	operation := "upsert"
	if e.DeletedAt != nil {
		operation = "delete"
	}
	return stx.EnqueueSync(context.Background(), "cash_close_events", e.ID.String(), operation, payload, e.Version)
}

func (r *CashCloseEventSQLiteRepository) CreateTransfer(tx sharedDomain.Transaction, tr *cashCloseDomain.CashTransfer) (*cashCloseDomain.CashTransfer, error) {
	if tr == nil {
		return nil, cashCloseDomain.ErrInvalidSessionState
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	_, err := stx.Exec(context.Background(), `INSERT INTO cash_transfers(
		id,gym_id,version,created_at,updated_at,deleted_at,session_id,drawer_id,
		destination,amount,transferred_at,transferred_by)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, tr.ID.String(), tr.GymID.String(), tr.Version,
		tr.CreatedAt.UnixMilli(), tr.UpdatedAt.UnixMilli(), nil, tr.SessionID.String(),
		tr.DrawerID.String(), tr.Destination, toCents(tr.Amount), tr.TransferredAt.UnixMilli(),
		tr.TransferredBy.String())
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"id": tr.ID.String(), "gym_id": tr.GymID.String(), "version": tr.Version,
		"created_at": tr.CreatedAt.UnixMilli(), "updated_at": tr.UpdatedAt.UnixMilli(), "deleted_at": nil,
		"session_id": tr.SessionID.String(), "drawer_id": tr.DrawerID.String(),
		"destination": tr.Destination, "amount": tr.Amount,
		"transferred_at": tr.TransferredAt.UnixMilli(), "transferred_by": tr.TransferredBy.String(),
	})
	if err != nil {
		return nil, err
	}
	if err := stx.EnqueueSync(context.Background(), "cash_transfers", tr.ID.String(), "upsert", payload, tr.Version); err != nil {
		return nil, err
	}
	return tr, nil
}

func cashCloseNullTime(v *time.Time) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v.UTC().UnixMilli(), Valid: true}
}

func cashCloseTimePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

func cashCloseNullString(v *string) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *v, Valid: true}
}

func cashCloseStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func cashCloseNullUUID(v *uuid.UUID) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: v.String(), Valid: true}
}

func cashCloseUUIDPtr(v sql.NullString) *uuid.UUID {
	if !v.Valid {
		return nil
	}
	id, err := uuid.Parse(v.String)
	if err != nil {
		return nil
	}
	return &id
}

func nullCashCloseInt(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func nullCashCloseString(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

func cashCloseMillis(v *time.Time) any {
	if v == nil {
		return nil
	}
	return v.UnixMilli()
}

func (r *CashCloseEventSQLiteRepository) LatestBefore(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID, date time.Time) (*cashCloseDomain.CashCloseEvent, error) {
	var row sqliteCashCloseEventRow
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &row,
		`SELECT * FROM cash_close_events WHERE gym_id=? AND drawer_id=? AND operational_date<? AND deleted_at IS NULL ORDER BY operational_date DESC,sequence DESC LIMIT 1`, gymID.String(), drawerID.String(), date.Format("2006-01-02"))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cashCloseEventFromRow(&row), nil
}

func (r *CashCloseSQLiteReader) CashEntries(tx sharedDomain.Transaction, q billingRepo.CashCloseQuery) ([]billingRepo.CashLedgerEntry, error) {
	type row struct {
		ID           string `db:"id"`
		RecordedAt   int64  `db:"recorded_at"`
		Amount       int64  `db:"amount"`
		Concept      string `db:"concept"`
		Reason       string `db:"reason"`
		OperatorName string `db:"operator_name"`
	}
	var rows []row
	err := tx.(*sharedDomain.SqlxTransaction).Select(context.Background(), &rows, `SELECT p.id AS id,p.created_at AS recorded_at,p.amount,p.concept,'' AS reason,COALESCE(u.full_name,'') AS operator_name
 FROM payments p LEFT JOIN users u ON u.id=p.operator_id AND u.gym_id=p.gym_id
 WHERE p.amount<>0 AND p.gym_id=? AND p.payment_date=? AND p.payment_method='cash' AND p.cash_destination<>'gym_fund' AND COALESCE(p.cash_drawer_id,p.gym_id)=? AND p.deleted_at IS NULL
 UNION ALL
 SELECT m.id AS id,m.created_at AS recorded_at,CASE WHEN m.movement_type='cash_out' THEN -m.amount ELSE m.amount END AS amount,m.movement_type AS concept,m.reason,COALESCE(u.full_name,'') AS operator_name
 FROM cash_movements m LEFT JOIN users u ON u.id=m.operator_id AND u.gym_id=m.gym_id
 WHERE m.gym_id=? AND m.movement_on=? AND COALESCE(m.cash_drawer_id,m.gym_id)=? AND m.deleted_at IS NULL
 ORDER BY recorded_at DESC,id`, q.GymID.String(), q.Date.Format("2006-01-02"), q.DrawerID.String(), q.GymID.String(), q.Date.Format("2006-01-02"), q.DrawerID.String())
	if err != nil {
		return nil, err
	}
	out := make([]billingRepo.CashLedgerEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, billingRepo.CashLedgerEntry{ID: uuid.MustParse(r.ID), RecordedAt: time.UnixMilli(r.RecordedAt).UTC(), Amount: fromCents(r.Amount), Concept: r.Concept, Reason: r.Reason, OperatorName: r.OperatorName})
	}
	return out, nil
}
