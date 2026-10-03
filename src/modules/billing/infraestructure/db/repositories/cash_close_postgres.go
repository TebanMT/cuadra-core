//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CashClosePostgresReader struct{}

func NewCashClosePostgresReader() *CashClosePostgresReader { return &CashClosePostgresReader{} }

// Aggregate runs the per-day rollup. Refunds (negative amounts) are NOT folded
// into ByMethod / ByConcept — they're surfaced in RefundTotal + RefundCount so
// the operator sees the gross take separately. ByConcept now carries counts;
// ByOperator brings the user.full_name + sales count via subqueries.
func (r *CashClosePostgresReader) Aggregate(tx sharedDomain.Transaction, q billingRepo.CashCloseQuery) (*billingRepo.CashCloseTotals, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	dateStr := q.Date.UTC().Format("2006-01-02")
	drawerID := q.DrawerID
	if drawerID == uuid.Nil {
		drawerID = q.GymID
	}

	type methodRow struct {
		PaymentMethod string
		Total         float64
	}
	var methodRows []methodRow
	if err := gormTx.Model(&models.PaymentModel{}).Where("amount <> 0").
		Select("payment_method, SUM(amount) AS total").
		Where("gym_id = ? AND payment_date = ? AND concept <> ? AND deleted_at IS NULL",
			q.GymID, dateStr, paymentDomain.ConceptRefund).
		Where("(payment_method<>'cash' OR (cash_destination<>'gym_fund' AND COALESCE(cash_drawer_id,gym_id)=?))", drawerID).
		Group("payment_method").Scan(&methodRows).Error; err != nil {
		return nil, err
	}

	type conceptRow struct {
		Concept string
		Total   float64
		Cnt     int
	}
	var conceptRows []conceptRow
	if err := gormTx.Model(&models.PaymentModel{}).Where("amount <> 0").
		Select("concept, SUM(amount) AS total, COUNT(*) AS cnt").
		Where("gym_id = ? AND payment_date = ? AND concept <> ? AND deleted_at IS NULL",
			q.GymID, dateStr, paymentDomain.ConceptRefund).
		Where("(payment_method<>'cash' OR (cash_destination<>'gym_fund' AND COALESCE(cash_drawer_id,gym_id)=?))", drawerID).
		Group("concept").Scan(&conceptRows).Error; err != nil {
		return nil, err
	}

	// Refunds agrupados por método: alimentan RefundTotal y RefundByMethod
	// (para descontar del cajón los refunds en efectivo). amount es negativo.
	type refundRow struct {
		PaymentMethod string
		Total         float64
		Cnt           int
	}
	var refundRows []refundRow
	if err := gormTx.Model(&models.PaymentModel{}).Where("amount <> 0").
		Select("payment_method, COALESCE(SUM(amount), 0) AS total, COUNT(*) AS cnt").
		Where("gym_id = ? AND payment_date = ? AND concept = ? AND deleted_at IS NULL",
			q.GymID, dateStr, paymentDomain.ConceptRefund).
		Where("(payment_method<>'cash' OR (cash_destination<>'gym_fund' AND COALESCE(cash_drawer_id,gym_id)=?))", drawerID).
		Group("payment_method").Scan(&refundRows).Error; err != nil {
		return nil, err
	}

	// Operator rollup: JOIN users so the FE renders names, and use
	// SUM(CASE WHEN concept='product' THEN 1 ELSE 0 END) for sales count.
	// Excludes refunds — those are visualized separately in the report.
	type opRow struct {
		OperatorID   uuid.UUID
		OperatorName *string
		Total        float64
		PaymentsN    int
		SalesN       int
	}
	var opRows []opRow
	if err := gormTx.Raw(`
		SELECT p.operator_id,
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
		q.GymID, dateStr, paymentDomain.ConceptRefund, drawerID).Scan(&opRows).Error; err != nil {
		return nil, err
	}

	out := &billingRepo.CashCloseTotals{
		ByMethod:       map[string]float64{},
		ByConcept:      map[string]billingRepo.ConceptTotal{},
		ByOperator:     make([]billingRepo.OperatorTotal, 0, len(opRows)),
		RefundByMethod: map[string]float64{},
	}
	for _, rf := range refundRows {
		out.RefundByMethod[rf.PaymentMethod] = rf.Total // negativo
		out.RefundTotal += rf.Total
		out.RefundCount += rf.Cnt
	}
	for _, m := range methodRows {
		out.ByMethod[m.PaymentMethod] = m.Total
		out.GrandTotal += m.Total
	}
	for _, c := range conceptRows {
		out.ByConcept[c.Concept] = billingRepo.ConceptTotal{Total: c.Total, Count: c.Cnt}
	}
	for _, o := range opRows {
		name := ""
		if o.OperatorName != nil {
			name = *o.OperatorName
		}
		out.ByOperator = append(out.ByOperator, billingRepo.OperatorTotal{
			OperatorID:   o.OperatorID,
			OperatorName: name,
			Total:        o.Total,
			PaymentsN:    o.PaymentsN,
			SalesN:       o.SalesN,
		})
	}
	return out, nil
}

func (r *CashClosePostgresReader) HasCashActivityAfter(tx sharedDomain.Transaction, q billingRepo.CashCloseQuery, after time.Time) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	drawerID := q.DrawerID
	if drawerID == uuid.Nil {
		drawerID = q.GymID
	}
	var found bool
	err := gormTx.Raw(`
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
		q.GymID, q.Date.UTC().Format("2006-01-02"), drawerID, after, after,
		q.GymID, q.Date.UTC().Format("2006-01-02"), drawerID, after, after).Scan(&found).Error
	return found, err
}

func (r *CashClosePostgresReader) SessionActivity(tx sharedDomain.Transaction, q billingRepo.CashSessionActivityQuery) (*billingRepo.CashSessionActivity, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	if q.DrawerID == uuid.Nil {
		q.DrawerID = q.GymID
	}
	if q.AsOf.IsZero() {
		q.AsOf = time.Now().UTC()
	}
	var row struct {
		Payments  float64
		CashIn    float64
		CashOut   float64
		Watermark *time.Time
	}
	err := g.Raw(`
		WITH physical AS (
			SELECT amount AS payment_amount, 0::numeric AS cash_in, 0::numeric AS cash_out,
			       created_at AS touched_at
			  FROM payments
			 WHERE amount<>0 AND gym_id=? AND payment_date=? AND payment_method='cash' AND cash_destination<>'gym_fund'
			   AND COALESCE(cash_drawer_id,gym_id)=? AND deleted_at IS NULL
			   AND created_at>=? AND created_at<=?
			UNION ALL
			SELECT 0::numeric,
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
		q.GymID, q.OperationalDate.UTC().Format("2006-01-02"), q.DrawerID, q.OpenedAt.UTC(), q.AsOf.UTC(),
		q.GymID, q.OperationalDate.UTC().Format("2006-01-02"), q.DrawerID, q.OpenedAt.UTC(), q.AsOf.UTC()).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	out := &billingRepo.CashSessionActivity{PaymentsCash: row.Payments, CashIn: row.CashIn, CashOut: row.CashOut}
	out.Net = out.PaymentsCash + out.CashIn - out.CashOut
	if row.Watermark != nil {
		out.Watermark = row.Watermark.UTC()
	}
	return out, nil
}

func (r *CashClosePostgresReader) CashDrawers(tx sharedDomain.Transaction, gymID uuid.UUID, date time.Time) ([]billingRepo.CashDrawerActivity, error) {
	var rows []struct {
		DrawerID       uuid.UUID
		ActivityCash   float64
		LastActivityAt time.Time
	}
	err := tx.(*sharedDomain.GormTransaction).Tx.Raw(`
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
		gymID, date.UTC().Format("2006-01-02"), gymID, date.UTC().Format("2006-01-02")).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]billingRepo.CashDrawerActivity, len(rows))
	for i, row := range rows {
		out[i] = billingRepo.CashDrawerActivity{
			DrawerID: row.DrawerID, ActivityCash: row.ActivityCash,
			LastActivityAt: row.LastActivityAt.UTC(),
		}
	}
	return out, nil
}

func (r *CashClosePostgresReader) SessionCoverage(tx sharedDomain.Transaction, q billingRepo.CashSessionCoverageQuery) (*billingRepo.CashSessionCoverage, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	var row struct {
		ActiveDays, ActiveSessions, MissingDays, UncoveredDays int
		OpenN, UnverifiedN, StaleN                             int
		ReconciledN, WithdrawnN, UnknownOpeningN               int
		AdjustedAfterWithdrawalN                               int
	}
	err := g.Raw(`
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
			       CASE WHEN s.status='stale' THEN TRUE
			         WHEN s.status IN ('open','withdrawn') THEN FALSE
			         WHEN ca.amount<>COALESCE(s.activity_cash,0) THEN TRUE
			         WHEN s.closed_at IS NOT NULL AND EXISTS (
			             SELECT 1 FROM physical p
			              WHERE p.day=s.operational_date AND p.drawer_id=s.drawer_id
			                AND p.recorded_at>=s.opened_at AND p.recorded_at>s.closed_at
			                AND (COALESCE(s.withdrawn_at, CASE WHEN s.cash_left IS NOT NULL THEN s.reconciled_at END) IS NULL OR p.recorded_at<=COALESCE(s.withdrawn_at, CASE WHEN s.cash_left IS NOT NULL THEN s.reconciled_at END))
			         ) THEN TRUE ELSE FALSE END AS derived_stale,
			       (s.adjusted_after_withdrawal OR (s.status='withdrawn'
			          AND ca.amount<>COALESCE(s.activity_cash,0))) AS derived_adjusted
			  FROM sessions s JOIN current_activity ca ON ca.id=s.id
		)
		SELECT (SELECT COUNT(*) FROM active_dates) AS active_days,
		       (SELECT COUNT(*) FROM sessions) AS active_sessions,
		       (SELECT COUNT(*) FROM active_dates a WHERE NOT EXISTS
		          (SELECT 1 FROM sessions s WHERE s.operational_date=a.day)) AS missing_days,
		       (SELECT COUNT(*) FROM uncovered_dates) AS uncovered_days,
		       (SELECT COUNT(*) FROM session_flags WHERE status='open') AS open_n,
		       (SELECT COUNT(*) FROM session_flags
		         WHERE status='closed_unverified' AND NOT derived_stale) AS unverified_n,
		       (SELECT COUNT(*) FROM session_flags WHERE derived_stale) AS stale_n,
		       (SELECT COUNT(*) FROM session_flags
		         WHERE status='reconciled' AND NOT derived_stale) AS reconciled_n,
		       (SELECT COUNT(*) FROM session_flags WHERE status='withdrawn') AS withdrawn_n,
		       (SELECT COUNT(*) FROM session_flags WHERE opening_cash_known=FALSE) AS unknown_opening_n,
		       (SELECT COUNT(*) FROM session_flags WHERE derived_adjusted) AS adjusted_after_withdrawal_n`,
		q.GymID, q.From, q.To, q.GymID, q.From, q.To, q.GymID, q.From, q.To).Scan(&row).Error
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
		out.ClosedUnverifiedSessions == 0 && out.StaleSessions == 0 && out.UnknownOpeningSessions == 0 &&
		out.AdjustedAfterWithdrawalSessions == 0
	return out, nil
}

type CashCloseEventPostgresRepository struct{}

func NewCashCloseEventPostgresRepository() *CashCloseEventPostgresRepository {
	return &CashCloseEventPostgresRepository{}
}

func (r *CashCloseEventPostgresRepository) Create(tx sharedDomain.Transaction, e *cashCloseDomain.CashCloseEvent) (*cashCloseDomain.CashCloseEvent, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := cashCloseEventToModel(e)
	if err := gormTx.Create(&row).Error; err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			(pgErr.ConstraintName == "uq_cash_sessions_natural" || pgErr.ConstraintName == "uq_cash_close_events_gym_date") {
			return nil, billingErrors.ErrCashCloseAlreadyExists
		}
		return nil, err
	}
	if err := emitCashCloseEvent(gormTx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (r *CashCloseEventPostgresRepository) Update(tx sharedDomain.Transaction, e *cashCloseDomain.CashCloseEvent) (*cashCloseDomain.CashCloseEvent, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	res := gormTx.Model(&models.CashCloseEventModel{}).
		Where("gym_id = ? AND id = ? AND version = ?", e.GymID, e.ID, e.Version-1).
		Updates(map[string]any{
			"version": e.Version, "updated_at": e.UpdatedAt, "deleted_at": e.DeletedAt,
			"drawer_id": e.DrawerID, "drawer_code": e.DrawerCode,
			"operational_date": e.OperationalDate, "sequence": e.Sequence, "status": e.Status,
			"close_date": e.CloseDate, "opening_cash": e.OpeningCash,
			"opening_cash_known": e.OpeningCashKnown, "activity_cash": e.ActivityCash,
			"calculated_cash": e.CalculatedCash, "counted_cash": e.CountedCash,
			"cash_left": e.CashLeft, "withdrawn_cash": e.WithdrawnCash,
			"withdrawal_destination": e.WithdrawalDestination,
			"discrepancy_reason":     e.DiscrepancyReason, "correction_reason": e.CorrectionReason,
			"adjusted_after_withdrawal": e.AdjustedAfterWithdrawal, "integrity_note": e.IntegrityNote,
			"opened_at": e.OpenedAt, "opened_by": e.OpenedBy, "closed_at": e.ClosedAt,
			"closed_by": e.ClosedBy, "reconciled_at": e.ReconciledAt,
			"reconciled_by": e.ReconciledBy, "stale_at": e.StaleAt,
			"withdrawn_at": e.WithdrawnAt, "withdrawn_by": e.WithdrawnBy,
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, billingErrors.ErrCashCloseConflict
	}
	if err := emitCashCloseEvent(gormTx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func emitCashCloseEvent(gormTx *gorm.DB, e *cashCloseDomain.CashCloseEvent) error {
	payload, err := json.Marshal(cashClosePayload(e))
	if err != nil {
		return err
	}
	return gormTx.Exec(`INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at,deleted_at) VALUES(?,'cash_close_events',?,?,?::jsonb,NOW(),?) ON CONFLICT(gym_id,entity_type,entity_id) DO UPDATE SET version=EXCLUDED.version,payload=EXCLUDED.payload,server_updated_at=NOW(),deleted_at=EXCLUDED.deleted_at`, e.GymID, e.ID, e.Version, string(payload), e.DeletedAt).Error
}

func cashClosePayload(e *cashCloseDomain.CashCloseEvent) map[string]any {
	return map[string]any{
		"id": e.ID, "gym_id": e.GymID, "version": e.Version,
		"created_at": e.CreatedAt.UnixMilli(), "updated_at": e.UpdatedAt.UnixMilli(), "deleted_at": e.DeletedAt,
		"drawer_id": e.DrawerID, "drawer_code": e.DrawerCode,
		"operational_date": e.OperationalDate.Format("2006-01-02"), "sequence": e.Sequence, "status": e.Status,
		"close_date": e.CloseDate.Format("2006-01-02"), "opening_cash": e.OpeningCash,
		"opening_cash_known": e.OpeningCashKnown, "activity_cash": e.ActivityCash,
		"calculated_cash": e.CalculatedCash, "counted_cash": e.CountedCash,
		"cash_left": e.CashLeft, "withdrawn_cash": e.WithdrawnCash,
		"withdrawal_destination": e.WithdrawalDestination,
		"discrepancy_reason":     e.DiscrepancyReason, "correction_reason": e.CorrectionReason,
		"adjusted_after_withdrawal": e.AdjustedAfterWithdrawal, "integrity_note": e.IntegrityNote,
		"opened_at": e.OpenedAt.UnixMilli(), "opened_by": e.OpenedBy,
		"closed_at": cashCloseTimeMillis(e.ClosedAt), "closed_by": e.ClosedBy,
		"reconciled_at": cashCloseTimeMillis(e.ReconciledAt), "reconciled_by": e.ReconciledBy,
		"stale_at": cashCloseTimeMillis(e.StaleAt), "withdrawn_at": cashCloseTimeMillis(e.WithdrawnAt),
		"withdrawn_by": e.WithdrawnBy,
	}
}

func cashCloseTimeMillis(v *time.Time) any {
	if v == nil {
		return nil
	}
	return v.UTC().UnixMilli()
}

func (r *CashCloseEventPostgresRepository) GetByID(tx sharedDomain.Transaction, gymID, id uuid.UUID) (*cashCloseDomain.CashCloseEvent, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	var row models.CashCloseEventModel
	err := g.Where("gym_id=? AND id=? AND deleted_at IS NULL", gymID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cashCloseEventFromModel(&row), nil
}

func (r *CashCloseEventPostgresRepository) GetByNaturalKey(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID, date time.Time, sequence int) (*cashCloseDomain.CashCloseEvent, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	var row models.CashCloseEventModel
	err := g.Where("gym_id=? AND drawer_id=? AND operational_date=? AND sequence=? AND deleted_at IS NULL",
		gymID, drawerID, date.Format("2006-01-02"), sequence).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cashCloseEventFromModel(&row), nil
}

func (r *CashCloseEventPostgresRepository) GetByDate(tx sharedDomain.Transaction, gymID uuid.UUID, date time.Time) (*cashCloseDomain.CashCloseEvent, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.CashCloseEventModel
	err := gormTx.Where("gym_id = ? AND drawer_id = ? AND operational_date = ? AND deleted_at IS NULL",
		gymID, cashCloseDomain.DefaultDrawerID(gymID), date.Format("2006-01-02")).
		Order("sequence DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cashCloseEventFromModel(&row), nil
}

func (r *CashCloseEventPostgresRepository) ListByDate(tx sharedDomain.Transaction, gymID uuid.UUID, date time.Time) ([]*cashCloseDomain.CashCloseEvent, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.CashCloseEventModel
	if err := g.Where("gym_id=? AND operational_date=? AND deleted_at IS NULL", gymID, date.Format("2006-01-02")).
		Order("drawer_code, sequence").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*cashCloseDomain.CashCloseEvent, len(rows))
	for i := range rows {
		out[i] = cashCloseEventFromModel(&rows[i])
	}
	return out, nil
}

func (r *CashCloseEventPostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]*cashCloseDomain.CashCloseEvent, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var rows []models.CashCloseEventModel
	if err := gormTx.Where("gym_id = ? AND deleted_at IS NULL", gymID).
		Order("operational_date DESC, sequence DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*cashCloseDomain.CashCloseEvent, len(rows))
	for i := range rows {
		out[i] = cashCloseEventFromModel(&rows[i])
	}
	return out, nil
}

func cashCloseEventToModel(e *cashCloseDomain.CashCloseEvent) models.CashCloseEventModel {
	return models.CashCloseEventModel{
		ID:                      e.ID,
		GymID:                   e.GymID,
		Version:                 e.Version,
		CreatedAt:               e.CreatedAt,
		UpdatedAt:               e.UpdatedAt,
		DeletedAt:               e.DeletedAt,
		DrawerID:                e.DrawerID,
		DrawerCode:              e.DrawerCode,
		OperationalDate:         e.OperationalDate,
		Sequence:                e.Sequence,
		Status:                  e.Status,
		CloseDate:               e.CloseDate,
		OpeningCash:             e.OpeningCash,
		OpeningCashKnown:        e.OpeningCashKnown,
		ActivityCash:            e.ActivityCash,
		CalculatedCash:          e.CalculatedCash,
		CountedCash:             e.CountedCash,
		CashLeft:                e.CashLeft,
		WithdrawnCash:           e.WithdrawnCash,
		WithdrawalDestination:   e.WithdrawalDestination,
		DiscrepancyReason:       e.DiscrepancyReason,
		CorrectionReason:        e.CorrectionReason,
		AdjustedAfterWithdrawal: e.AdjustedAfterWithdrawal,
		IntegrityNote:           e.IntegrityNote,
		OpenedAt:                e.OpenedAt,
		OpenedBy:                e.OpenedBy,
		ClosedAt:                e.ClosedAt,
		ClosedBy:                e.ClosedBy,
		ReconciledAt:            e.ReconciledAt,
		ReconciledBy:            e.ReconciledBy,
		StaleAt:                 e.StaleAt,
		WithdrawnAt:             e.WithdrawnAt,
		WithdrawnBy:             e.WithdrawnBy,
	}
}

func cashCloseEventFromModel(m *models.CashCloseEventModel) *cashCloseDomain.CashCloseEvent {
	return &cashCloseDomain.CashCloseEvent{
		ID:                      m.ID,
		GymID:                   m.GymID,
		Version:                 m.Version,
		DrawerID:                m.DrawerID,
		DrawerCode:              m.DrawerCode,
		OperationalDate:         dayDate(m.OperationalDate),
		Sequence:                m.Sequence,
		Status:                  m.Status,
		CloseDate:               time.Date(m.CloseDate.Year(), m.CloseDate.Month(), m.CloseDate.Day(), 0, 0, 0, 0, time.UTC),
		OpeningCash:             m.OpeningCash,
		OpeningCashKnown:        m.OpeningCashKnown,
		ActivityCash:            m.ActivityCash,
		CalculatedCash:          m.CalculatedCash,
		CountedCash:             m.CountedCash,
		CashLeft:                m.CashLeft,
		WithdrawnCash:           m.WithdrawnCash,
		WithdrawalDestination:   m.WithdrawalDestination,
		DiscrepancyReason:       m.DiscrepancyReason,
		CorrectionReason:        m.CorrectionReason,
		AdjustedAfterWithdrawal: m.AdjustedAfterWithdrawal,
		IntegrityNote:           m.IntegrityNote,
		OpenedAt:                m.OpenedAt,
		OpenedBy:                m.OpenedBy,
		ClosedAt:                m.ClosedAt,
		ClosedBy:                m.ClosedBy,
		ReconciledAt:            m.ReconciledAt,
		ReconciledBy:            m.ReconciledBy,
		StaleAt:                 m.StaleAt,
		WithdrawnAt:             m.WithdrawnAt,
		WithdrawnBy:             m.WithdrawnBy,
		CreatedAt:               m.CreatedAt,
		UpdatedAt:               m.UpdatedAt,
		DeletedAt:               m.DeletedAt,
	}
}

func dayDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (r *CashCloseEventPostgresRepository) CreateTransfer(tx sharedDomain.Transaction, tr *cashCloseDomain.CashTransfer) (*cashCloseDomain.CashTransfer, error) {
	if tr == nil {
		return nil, cashCloseDomain.ErrInvalidSessionState
	}
	g := tx.(*sharedDomain.GormTransaction).Tx
	row := models.CashTransferModel{ID: tr.ID, GymID: tr.GymID, Version: tr.Version,
		CreatedAt: tr.CreatedAt, UpdatedAt: tr.UpdatedAt, DeletedAt: tr.DeletedAt,
		SessionID: tr.SessionID, DrawerID: tr.DrawerID, Destination: tr.Destination,
		Amount: tr.Amount, TransferredAt: tr.TransferredAt, TransferredBy: tr.TransferredBy}
	if err := g.Create(&row).Error; err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"id": tr.ID, "gym_id": tr.GymID, "version": tr.Version,
		"created_at": tr.CreatedAt.UnixMilli(), "updated_at": tr.UpdatedAt.UnixMilli(), "deleted_at": tr.DeletedAt,
		"session_id": tr.SessionID, "drawer_id": tr.DrawerID, "destination": tr.Destination,
		"amount": tr.Amount, "transferred_at": tr.TransferredAt.UnixMilli(), "transferred_by": tr.TransferredBy,
	})
	if err != nil {
		return nil, err
	}
	if err := g.Exec(`INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at,deleted_at)
		VALUES(?,'cash_transfers',?,?,?::jsonb,NOW(),?)
		ON CONFLICT(gym_id,entity_type,entity_id) DO UPDATE SET version=EXCLUDED.version,payload=EXCLUDED.payload,server_updated_at=NOW(),deleted_at=EXCLUDED.deleted_at`,
		tr.GymID, tr.ID, tr.Version, string(payload), tr.DeletedAt).Error; err != nil {
		return nil, err
	}
	return tr, nil
}

func (r *CashCloseEventPostgresRepository) LatestBefore(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID, date time.Time) (*cashCloseDomain.CashCloseEvent, error) {
	var row models.CashCloseEventModel
	err := tx.(*sharedDomain.GormTransaction).Tx.Where("gym_id=? AND drawer_id=? AND operational_date<? AND deleted_at IS NULL", gymID, drawerID, date.Format("2006-01-02")).Order("operational_date DESC,sequence DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cashCloseEventFromModel(&row), nil
}

func (r *CashClosePostgresReader) CashEntries(tx sharedDomain.Transaction, q billingRepo.CashCloseQuery) ([]billingRepo.CashLedgerEntry, error) {
	out := []billingRepo.CashLedgerEntry{}
	err := tx.(*sharedDomain.GormTransaction).Tx.Raw(`SELECT p.id AS id,p.created_at AS recorded_at,p.amount,p.concept,'' AS reason,COALESCE(u.full_name,'') AS operator_name
 FROM payments p LEFT JOIN users u ON u.id=p.operator_id AND u.gym_id=p.gym_id
 WHERE p.amount<>0 AND p.gym_id=? AND p.payment_date=? AND p.payment_method='cash' AND p.cash_destination<>'gym_fund' AND COALESCE(p.cash_drawer_id,p.gym_id)=? AND p.deleted_at IS NULL
 UNION ALL
 SELECT m.id AS id,m.created_at AS recorded_at,CASE WHEN m.movement_type='cash_out' THEN -m.amount ELSE m.amount END AS amount,m.movement_type AS concept,m.reason,COALESCE(u.full_name,'') AS operator_name
 FROM cash_movements m LEFT JOIN users u ON u.id=m.operator_id AND u.gym_id=m.gym_id
 WHERE m.gym_id=? AND m.movement_on=? AND COALESCE(m.cash_drawer_id,m.gym_id)=? AND m.deleted_at IS NULL
 ORDER BY recorded_at DESC,id`, q.GymID, q.Date.Format("2006-01-02"), q.DrawerID, q.GymID, q.Date.Format("2006-01-02"), q.DrawerID).Scan(&out).Error
	return out, err
}
