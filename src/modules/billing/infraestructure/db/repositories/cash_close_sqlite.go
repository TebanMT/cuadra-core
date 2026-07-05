//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
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

	type methodRow struct {
		PaymentMethod string `db:"payment_method"`
		Total         int64  `db:"total"`
	}
	var methodRows []methodRow
	if err := stx.Select(context.Background(), &methodRows,
		`SELECT payment_method, COALESCE(SUM(amount), 0) AS total FROM payments
		 WHERE gym_id = ? AND payment_date = ? AND concept <> ? AND deleted_at IS NULL
		 GROUP BY payment_method`,
		q.GymID.String(), dateStr, paymentDomain.ConceptRefund); err != nil {
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
		 WHERE gym_id = ? AND payment_date = ? AND concept <> ? AND deleted_at IS NULL
		 GROUP BY concept`,
		q.GymID.String(), dateStr, paymentDomain.ConceptRefund); err != nil {
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
		 WHERE gym_id = ? AND payment_date = ? AND concept = ? AND deleted_at IS NULL
		 GROUP BY payment_method`,
		q.GymID.String(), dateStr, paymentDomain.ConceptRefund); err != nil {
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
		 WHERE p.gym_id = ? AND p.payment_date = ?
		   AND p.concept <> ? AND p.deleted_at IS NULL
		 GROUP BY p.operator_id, u.full_name
		 ORDER BY total DESC`,
		q.GymID.String(), dateStr, paymentDomain.ConceptRefund); err != nil {
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

type CashCloseEventSQLiteRepository struct{}

func NewCashCloseEventSQLiteRepository() *CashCloseEventSQLiteRepository {
	return &CashCloseEventSQLiteRepository{}
}

type sqliteCashCloseEventRow struct {
	ID                string         `db:"id"`
	GymID             string         `db:"gym_id"`
	Version           int            `db:"version"`
	CreatedAt         int64          `db:"created_at"`
	UpdatedAt         int64          `db:"updated_at"`
	DeletedAt         sql.NullInt64  `db:"deleted_at"`
	SyncedAt          sql.NullInt64  `db:"synced_at"`
	CloseDate         string         `db:"close_date"`
	CalculatedCash    int64          `db:"calculated_cash"`
	CountedCash       sql.NullInt64  `db:"counted_cash"`
	Discrepancy       sql.NullInt64  `db:"discrepancy"`
	DiscrepancyReason sql.NullString `db:"discrepancy_reason"`
	ClosedBy          string         `db:"closed_by"`
}

func (r *CashCloseEventSQLiteRepository) Create(tx sharedDomain.Transaction, e *cashCloseDomain.CashCloseEvent) (*cashCloseDomain.CashCloseEvent, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := cashCloseEventToRow(e)
	const stmt = `
		INSERT INTO cash_close_events (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    close_date, calculated_cash, counted_cash, discrepancy_reason, closed_by
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :close_date, :calculated_cash, :counted_cash, :discrepancy_reason, :closed_by
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueCashCloseEvent(stx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (r *CashCloseEventSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]*cashCloseDomain.CashCloseEvent, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var rows []sqliteCashCloseEventRow
	if err := stx.Select(context.Background(), &rows,
		`SELECT * FROM cash_close_events WHERE gym_id = ? AND deleted_at IS NULL
		 ORDER BY close_date DESC LIMIT ?`, gymID.String(), limit); err != nil {
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
		ID:             e.ID.String(),
		GymID:          e.GymID.String(),
		Version:        e.Version,
		CreatedAt:      e.CreatedAt.UnixMilli(),
		UpdatedAt:      e.UpdatedAt.UnixMilli(),
		CloseDate:      e.CloseDate.UTC().Format("2006-01-02"),
		CalculatedCash: toCents(e.CalculatedCash),
		ClosedBy:       e.ClosedBy.String(),
	}
	if e.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: e.DeletedAt.UnixMilli(), Valid: true}
	}
	if e.CountedCash != nil {
		row.CountedCash = sql.NullInt64{Int64: toCents(*e.CountedCash), Valid: true}
	}
	if e.DiscrepancyReason != nil {
		row.DiscrepancyReason = sql.NullString{String: *e.DiscrepancyReason, Valid: true}
	}
	return row
}

func cashCloseEventFromRow(r *sqliteCashCloseEventRow) *cashCloseDomain.CashCloseEvent {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	closedBy, _ := uuid.Parse(r.ClosedBy)
	closeDate, _ := time.Parse("2006-01-02", r.CloseDate)
	e := &cashCloseDomain.CashCloseEvent{
		ID:             id,
		GymID:          gymID,
		Version:        r.Version,
		CloseDate:      closeDate,
		CalculatedCash: fromCents(r.CalculatedCash),
		ClosedBy:       closedBy,
		CreatedAt:      time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:      time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		e.DeletedAt = &t
	}
	if r.CountedCash.Valid {
		v := fromCents(r.CountedCash.Int64)
		e.CountedCash = &v
	}
	if r.DiscrepancyReason.Valid {
		s := r.DiscrepancyReason.String
		e.DiscrepancyReason = &s
	}
	return e
}

func enqueueCashCloseEvent(stx *sharedDomain.SqlxTransaction, e *cashCloseDomain.CashCloseEvent) error {
	if stx.Queue == nil {
		return nil
	}
	var counted float64
	if e.CountedCash != nil {
		counted = *e.CountedCash
	}
	reason := ""
	if e.DiscrepancyReason != nil {
		reason = *e.DiscrepancyReason
	}
	payload, err := json.Marshal(map[string]any{
		"id":                 e.ID.String(),
		"gym_id":             e.GymID.String(),
		"version":            e.Version,
		"close_date":         e.CloseDate.UTC().Format("2006-01-02"),
		"calculated_cash":    e.CalculatedCash,
		"counted_cash":       counted,
		"discrepancy_reason": reason,
		"closed_by":          e.ClosedBy.String(),
		"created_at":         e.CreatedAt.UnixMilli(),
		"updated_at":         e.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "cash_close_events", e.ID.String(), "upsert", payload, e.Version)
}
