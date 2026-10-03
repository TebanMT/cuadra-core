//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

const dateLayout = "2006-01-02"

// SQLite stores money as cents (ADR-002 §2). Convert at the edge.
func toCents(v float64) int64   { return int64(math.Round(v * 100)) }
func fromCents(c int64) float64 { return float64(c) / 100 }

type PaymentSQLiteRepository struct{}

func NewPaymentSQLiteRepository() *PaymentSQLiteRepository {
	return &PaymentSQLiteRepository{}
}

type sqlitePaymentRow struct {
	ID                     string         `db:"id"`
	GymID                  string         `db:"gym_id"`
	Version                int            `db:"version"`
	CreatedAt              int64          `db:"created_at"`
	UpdatedAt              int64          `db:"updated_at"`
	DeletedAt              sql.NullInt64  `db:"deleted_at"`
	SyncedAt               sql.NullInt64  `db:"synced_at"`
	Folio                  string         `db:"folio"`
	MemberID               sql.NullString `db:"member_id"`
	MembershipID           sql.NullString `db:"membership_id"`
	IdempotencyKey         sql.NullString `db:"idempotency_key"`
	IdempotencyFingerprint sql.NullString `db:"idempotency_fingerprint"`
	IdempotencyResult      sql.NullString `db:"idempotency_result"`
	Amount                 int64          `db:"amount"`
	RecognizedAmount       int64          `db:"recognized_amount"`
	PaymentMethod          string         `db:"payment_method"`
	CashDestination        string         `db:"cash_destination"`
	CashDrawerID           sql.NullString `db:"cash_drawer_id"`
	Concept                string         `db:"concept"`
	ParentPaymentID        sql.NullString `db:"parent_payment_id"`
	DiscountAmount         int64          `db:"discount_amount"`
	DiscountReason         sql.NullString `db:"discount_reason"`
	BalancePending         int64          `db:"balance_pending"`
	PaymentDate            string         `db:"payment_date"`
	Notes                  sql.NullString `db:"notes"`
	Breakdown              sql.NullString `db:"breakdown"`
	OperatorID             string         `db:"operator_id"`
}

func (r *PaymentSQLiteRepository) Create(tx sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := paymentToRow(p)
	const stmt = `
		INSERT INTO payments (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    folio, member_id, membership_id, idempotency_key, idempotency_fingerprint, idempotency_result,
		    amount, recognized_amount, payment_method, cash_drawer_id, cash_destination, concept, parent_payment_id,
		    discount_amount, discount_reason, balance_pending,
		    payment_date, notes, breakdown, operator_id
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :folio, :member_id, :membership_id, :idempotency_key, :idempotency_fingerprint, :idempotency_result,
		    :amount, :recognized_amount, :payment_method, :cash_drawer_id, :cash_destination, :concept, :parent_payment_id,
		    :discount_amount, :discount_reason, :balance_pending,
		    :payment_date, :notes, :breakdown, :operator_id
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueuePayment(stx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Update is restricted to notes + balance_pending — payments are append-only.
func (r *PaymentSQLiteRepository) Update(tx sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	p.UpdatedAt = time.Now().UTC()
	row := paymentToRow(p)
	const stmt = `
		UPDATE payments SET
		    version = :version, updated_at = :updated_at,
		    notes = :notes, balance_pending = :balance_pending
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueuePayment(stx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *PaymentSQLiteRepository) UpdateForSaleCorrection(tx sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := paymentToRow(p)
	const stmt = `UPDATE payments SET version=:version,updated_at=:updated_at,
		amount=:amount,recognized_amount=:recognized_amount,balance_pending=:balance_pending,
		discount_amount=:discount_amount,breakdown=:breakdown,deleted_at=:deleted_at
		WHERE gym_id=:gym_id AND id=:id AND deleted_at IS NULL`
	res, err := stx.NamedExec(context.Background(), stmt, row)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrPaymentNotFound, "")
	}
	if err := enqueuePayment(stx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *PaymentSQLiteRepository) UpdateForAdministrativeCorrection(tx sharedDomain.Transaction, p *paymentDomain.Payment, expectedVersion int) (*paymentDomain.Payment, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := paymentToRow(p)
	res, err := stx.NamedExec(context.Background(), `UPDATE payments SET
		version=:version,updated_at=:updated_at,amount=:amount,recognized_amount=:recognized_amount,
		balance_pending=:balance_pending,payment_method=:payment_method,cash_drawer_id=:cash_drawer_id,cash_destination=:cash_destination,
		payment_date=:payment_date,deleted_at=:deleted_at
		WHERE gym_id=:gym_id AND id=:id AND version=:expected_version AND deleted_at IS NULL`, map[string]any{
		"version": row.Version, "updated_at": row.UpdatedAt, "amount": row.Amount,
		"recognized_amount": row.RecognizedAmount, "balance_pending": row.BalancePending,
		"cash_destination": row.CashDestination, "payment_method": row.PaymentMethod, "cash_drawer_id": row.CashDrawerID,
		"payment_date": row.PaymentDate, "deleted_at": row.DeletedAt,
		"gym_id": row.GymID, "id": row.ID,
		"expected_version": expectedVersion,
	})
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrPaymentVersionConflict, "")
	}
	if err := enqueuePayment(stx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *PaymentSQLiteRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*paymentDomain.Payment, error) {
	var row sqlitePaymentRow
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &row,
		`SELECT * FROM payments WHERE gym_id=? AND idempotency_key=? AND deleted_at IS NULL`, gymID.String(), strings.TrimSpace(key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return paymentFromRow(&row), nil
}

func (r *PaymentSQLiteRepository) FinalizeIdempotency(tx sharedDomain.Transaction, p *paymentDomain.Payment) error {
	row := paymentToRow(p)
	res, err := tx.(*sharedDomain.SqlxTransaction).NamedExec(context.Background(), `UPDATE payments SET
		version=:version,updated_at=:updated_at,idempotency_result=:idempotency_result
		WHERE gym_id=:gym_id AND id=:id AND idempotency_key=:idempotency_key
		  AND idempotency_fingerprint=:idempotency_fingerprint AND deleted_at IS NULL`, row)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return billingErrors.ErrIdempotencyKeyConflict
	}
	return enqueuePayment(tx.(*sharedDomain.SqlxTransaction), p)
}

func (r *PaymentSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*paymentDomain.Payment, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqlitePaymentRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM payments WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrPaymentNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return paymentFromRow(&row), nil
}

func (r *PaymentSQLiteRepository) ListByMember(tx sharedDomain.Transaction, q billingRepo.ListByMemberQuery) ([]*paymentDomain.Payment, int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	page, pageSize := normalizePage(q.Page, q.PageSize)
	where := []string{"gym_id = ?", "member_id = ?", "deleted_at IS NULL"}
	args := []any{q.GymID.String(), q.MemberID.String()}
	if q.ConceptFilter != "" {
		where = append(where, "concept = ?")
		args = append(args, q.ConceptFilter)
	}
	if q.From != nil {
		where = append(where, "payment_date >= ?")
		args = append(args, q.From.UTC().Format(dateLayout))
	}
	if q.To != nil {
		where = append(where, "payment_date <= ?")
		args = append(args, q.To.UTC().Format(dateLayout))
	}
	whereClause := strings.Join(where, " AND ")
	var total int
	if err := stx.Get(context.Background(), &total,
		`SELECT COUNT(*) FROM payments WHERE `+whereClause, args...); err != nil {
		return nil, 0, err
	}
	q2 := fmt.Sprintf(
		`SELECT * FROM payments WHERE %s ORDER BY payment_date DESC, created_at DESC LIMIT %d OFFSET %d`,
		whereClause, pageSize, (page-1)*pageSize)
	var rows []sqlitePaymentRow
	if err := stx.Select(context.Background(), &rows, q2, args...); err != nil {
		return nil, 0, err
	}
	out := make([]*paymentDomain.Payment, len(rows))
	for i := range rows {
		out[i] = paymentFromRow(&rows[i])
	}
	return out, total, nil
}

// SumPendingByMember — agregado del lado del repo (ver interfaz). SQLite
// guarda centavos; el dominio habla pesos → fromCents a la salida.
func (r *PaymentSQLiteRepository) SumPendingByMember(tx sharedDomain.Transaction, gymID, memberID uuid.UUID) (float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var cents int64
	err := stx.Get(context.Background(), &cents,
		`SELECT COALESCE(SUM(balance_pending), 0) FROM payments
		  WHERE gym_id = ? AND member_id = ? AND deleted_at IS NULL`,
		gymID.String(), memberID.String())
	return fromCents(cents), err
}

func (r *PaymentSQLiteRepository) ListByGymBetweenDates(tx sharedDomain.Transaction, q billingRepo.ListByGymQuery) ([]*paymentDomain.Payment, int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	page, pageSize := normalizePage(q.Page, q.PageSize)
	where := []string{"gym_id = ?", "deleted_at IS NULL", "payment_date >= ?", "payment_date <= ?"}
	args := []any{q.GymID.String(), q.From.UTC().Format(dateLayout), q.To.UTC().Format(dateLayout)}
	if q.ConceptFilter != "" {
		where = append(where, "concept = ?")
		args = append(args, q.ConceptFilter)
	}
	if q.MethodFilter != "" {
		where = append(where, "payment_method = ?")
		args = append(args, q.MethodFilter)
	}
	whereClause := strings.Join(where, " AND ")
	var total int
	if err := stx.Get(context.Background(), &total,
		`SELECT COUNT(*) FROM payments WHERE `+whereClause, args...); err != nil {
		return nil, 0, err
	}
	q2 := fmt.Sprintf(
		`SELECT * FROM payments WHERE %s ORDER BY payment_date DESC, created_at DESC LIMIT %d OFFSET %d`,
		whereClause, pageSize, (page-1)*pageSize)
	var rows []sqlitePaymentRow
	if err := stx.Select(context.Background(), &rows, q2, args...); err != nil {
		return nil, 0, err
	}
	out := make([]*paymentDomain.Payment, len(rows))
	for i := range rows {
		out[i] = paymentFromRow(&rows[i])
	}
	return out, total, nil
}

func (r *PaymentSQLiteRepository) AggregateByGymBetweenDates(tx sharedDomain.Transaction, q billingRepo.ListByGymQuery) (billingRepo.ListByGymAggregates, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	where := []string{"gym_id = ?", "deleted_at IS NULL", "payment_date >= ?", "payment_date <= ?"}
	args := []any{q.GymID.String(), q.From.UTC().Format(dateLayout), q.To.UTC().Format(dateLayout)}
	if q.ConceptFilter != "" {
		where = append(where, "concept = ?")
		args = append(args, q.ConceptFilter)
	}
	if q.MethodFilter != "" {
		where = append(where, "payment_method = ?")
		args = append(args, q.MethodFilter)
	}
	var row struct {
		NetTotal      int64 `db:"net_total"`
		RefundTotal   int64 `db:"refund_total"`
		CashTotal     int64 `db:"cash_total"`
		TransferTotal int64 `db:"transfer_total"`
		CardTotal     int64 `db:"card_total"`
	}
	// amount está en cents (INTEGER); /100 al edge como el resto del repo.
	if err := stx.Get(context.Background(), &row, `
		SELECT
		  COALESCE(SUM(amount), 0) AS net_total,
		  COALESCE(SUM(CASE WHEN concept = 'refund' THEN ABS(amount) ELSE 0 END), 0) AS refund_total,
		  COALESCE(SUM(CASE WHEN payment_method = 'cash' THEN amount ELSE 0 END), 0) AS cash_total,
		  COALESCE(SUM(CASE WHEN payment_method = 'transfer' THEN amount ELSE 0 END), 0) AS transfer_total,
		  COALESCE(SUM(CASE WHEN payment_method = 'card' THEN amount ELSE 0 END), 0) AS card_total
		FROM payments WHERE `+strings.Join(where, " AND "), args...); err != nil {
		return billingRepo.ListByGymAggregates{}, err
	}
	return billingRepo.ListByGymAggregates{
		NetTotal:      float64(row.NetTotal) / 100,
		RefundTotal:   float64(row.RefundTotal) / 100,
		CashTotal:     float64(row.CashTotal) / 100,
		TransferTotal: float64(row.TransferTotal) / 100,
		CardTotal:     float64(row.CardTotal) / 100,
	}, nil
}

func (r *PaymentSQLiteRepository) HasRefundFor(tx sharedDomain.Transaction, parentPaymentID uuid.UUID) (bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(*) FROM payments WHERE parent_payment_id = ? AND concept = ? AND deleted_at IS NULL`,
		parentPaymentID.String(), paymentDomain.ConceptRefund)
	return n > 0, err
}

func (r *PaymentSQLiteRepository) RefundBalanceForUpdate(tx sharedDomain.Transaction, gymID, rootPaymentID uuid.UUID) (billingRepo.RefundBalance, error) {
	return r.refundBalance(tx, gymID, rootPaymentID)
}

func (r *PaymentSQLiteRepository) RefundBalance(tx sharedDomain.Transaction, gymID, rootPaymentID uuid.UUID) (billingRepo.RefundBalance, error) {
	return r.refundBalance(tx, gymID, rootPaymentID)
}

func (r *PaymentSQLiteRepository) refundBalance(tx sharedDomain.Transaction, gymID, rootPaymentID uuid.UUID) (billingRepo.RefundBalance, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var root sqlitePaymentRow
	err := stx.Get(context.Background(), &root,
		`SELECT * FROM payments WHERE gym_id=? AND id=? AND deleted_at IS NULL`, gymID.String(), rootPaymentID.String())
	if errors.Is(err, sql.ErrNoRows) {
		return billingRepo.RefundBalance{}, sharedDomain.NewBusinessError(billingErrors.ErrPaymentNotFound, "")
	}
	if err != nil {
		return billingRepo.RefundBalance{}, err
	}
	var sums struct {
		Collected      int64 `db:"collected"`
		Recognized     int64 `db:"recognized"`
		Refunded       int64 `db:"refunded"`
		SourceRefunded int64 `db:"source_refunded"`
	}
	if err := stx.Get(context.Background(), &sums, `
		SELECT ? + COALESCE(SUM(CASE WHEN p.concept='balance_settlement' THEN p.amount ELSE 0 END),0) AS collected,
		       ? + COALESCE(SUM(CASE WHEN p.concept='balance_settlement' THEN p.recognized_amount ELSE 0 END),0) AS recognized,
		       COALESCE(SUM(CASE WHEN p.concept='refund' THEN ABS(p.amount) ELSE 0 END),0) AS refunded,
		       COALESCE(SUM(CASE WHEN p.concept='refund' AND p.parent_payment_id=? THEN ABS(p.amount) ELSE 0 END),0) AS source_refunded
		FROM payments p
		WHERE p.gym_id=? AND p.deleted_at IS NULL AND (
		  p.parent_payment_id=? OR (
		    p.concept='refund' AND EXISTS(
		      SELECT 1 FROM payments source
		      WHERE source.id=p.parent_payment_id AND source.gym_id=p.gym_id
		        AND source.concept='balance_settlement'
		        AND source.parent_payment_id=? AND source.deleted_at IS NULL
		    )
		  )
		)`,
		root.Amount, root.RecognizedAmount, rootPaymentID.String(), gymID.String(), rootPaymentID.String(), rootPaymentID.String()); err != nil {
		return billingRepo.RefundBalance{}, err
	}
	refundable := sums.Collected - sums.Refunded
	if refundable < 0 {
		refundable = 0
	}
	sourceRefundable := root.Amount - sums.SourceRefunded
	if sourceRefundable < 0 {
		sourceRefundable = 0
	}
	return billingRepo.RefundBalance{
		Root: paymentFromRow(&root), Collected: fromCents(sums.Collected), Recognized: fromCents(sums.Recognized),
		Refunded: fromCents(sums.Refunded), Refundable: fromCents(refundable),
		SourceRefunded: fromCents(sums.SourceRefunded), SourceRefundable: fromCents(sourceRefundable),
	}, nil
}

func (r *PaymentSQLiteRepository) MaxFolioForConcept(tx sharedDomain.Transaction, gymID uuid.UUID, concept string) (string, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var max sql.NullString
	// Ordena por el valor NUMÉRICO del folio (lo de después del '-'), no
	// lexicográfico: MAX(folio) como string daba "MEM-99999" > "MEM-100000"
	// (porque '9' > '1'), así que el folio siguiente calculado (100000)
	// colisionaba con uno ya existente — caso típico tras importar pagos con
	// MEM-%05d de IDs legacy que pasan de 99999.
	err := stx.Get(context.Background(), &max,
		`SELECT folio FROM payments
		   WHERE gym_id = ? AND concept = ?
		   ORDER BY CAST(SUBSTR(folio, INSTR(folio, '-') + 1) AS INTEGER) DESC, folio DESC
		   LIMIT 1`,
		gymID.String(), concept)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if !max.Valid {
		return "", nil
	}
	return max.String, nil
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func paymentToRow(p *paymentDomain.Payment) sqlitePaymentRow {
	row := sqlitePaymentRow{
		ID:               p.ID.String(),
		GymID:            p.GymID.String(),
		Version:          p.Version,
		CreatedAt:        p.CreatedAt.UnixMilli(),
		UpdatedAt:        p.UpdatedAt.UnixMilli(),
		Folio:            p.Folio,
		Amount:           toCents(p.Amount),
		RecognizedAmount: toCents(p.RecognizedAmount),
		CashDestination:  p.EffectiveCashDestination(),
		PaymentMethod:    p.PaymentMethod,
		Concept:          p.Concept,
		DiscountAmount:   toCents(p.DiscountAmount),
		BalancePending:   toCents(p.BalancePending),
		PaymentDate:      p.PaymentDate.UTC().Format(dateLayout),
		OperatorID:       p.OperatorID.String(),
	}
	if p.PaymentMethod == paymentDomain.MethodCash && p.EffectiveCashDestination() != "gym_fund" {
		drawerID := p.GymID
		if p.CashDrawerID != nil && *p.CashDrawerID != uuid.Nil {
			drawerID = *p.CashDrawerID
		}
		row.CashDrawerID = sql.NullString{String: drawerID.String(), Valid: true}
	}
	if p.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: p.DeletedAt.UnixMilli(), Valid: true}
	}
	if p.MemberID != nil {
		row.MemberID = sql.NullString{String: p.MemberID.String(), Valid: true}
	}
	if p.MembershipID != nil {
		row.MembershipID = sql.NullString{String: p.MembershipID.String(), Valid: true}
	}
	if p.IdempotencyKey != "" {
		row.IdempotencyKey = sql.NullString{String: p.IdempotencyKey, Valid: true}
		row.IdempotencyFingerprint = sql.NullString{String: p.IdempotencyFingerprint, Valid: true}
	}
	if len(p.IdempotencyResult) > 0 {
		row.IdempotencyResult = sql.NullString{String: string(p.IdempotencyResult), Valid: true}
	}
	if p.ParentPaymentID != nil {
		row.ParentPaymentID = sql.NullString{String: p.ParentPaymentID.String(), Valid: true}
	}
	if p.DiscountReason != nil {
		row.DiscountReason = sql.NullString{String: *p.DiscountReason, Valid: true}
	}
	if p.Notes != nil {
		row.Notes = sql.NullString{String: *p.Notes, Valid: true}
	}
	if len(p.Breakdown) > 0 {
		if b, err := json.Marshal(p.Breakdown); err == nil {
			row.Breakdown = sql.NullString{String: string(b), Valid: true}
		}
	}
	return row
}

func paymentFromRow(r *sqlitePaymentRow) *paymentDomain.Payment {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	opID, _ := uuid.Parse(r.OperatorID)
	paymentDate, _ := time.Parse(dateLayout, r.PaymentDate)
	p := &paymentDomain.Payment{
		ID:               id,
		GymID:            gymID,
		Version:          r.Version,
		Folio:            r.Folio,
		Amount:           fromCents(r.Amount),
		RecognizedAmount: fromCents(r.RecognizedAmount),
		CashDestination:  r.CashDestination,
		PaymentMethod:    r.PaymentMethod,
		Concept:          r.Concept,
		DiscountAmount:   fromCents(r.DiscountAmount),
		BalancePending:   fromCents(r.BalancePending),
		PaymentDate:      paymentDate,
		OperatorID:       opID,
		CreatedAt:        time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:        time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		p.DeletedAt = &t
	}
	if r.MemberID.Valid {
		mid, _ := uuid.Parse(r.MemberID.String)
		p.MemberID = &mid
	}
	if r.MembershipID.Valid {
		membershipID, _ := uuid.Parse(r.MembershipID.String)
		p.MembershipID = &membershipID
	}
	if r.IdempotencyKey.Valid {
		p.IdempotencyKey = r.IdempotencyKey.String
		p.IdempotencyFingerprint = r.IdempotencyFingerprint.String
	}
	if r.IdempotencyResult.Valid {
		p.IdempotencyResult = append([]byte(nil), r.IdempotencyResult.String...)
	}
	if r.ParentPaymentID.Valid {
		pid, _ := uuid.Parse(r.ParentPaymentID.String)
		p.ParentPaymentID = &pid
	}
	if r.CashDrawerID.Valid {
		drawerID, _ := uuid.Parse(r.CashDrawerID.String)
		p.CashDrawerID = &drawerID
	}
	if r.DiscountReason.Valid {
		v := r.DiscountReason.String
		p.DiscountReason = &v
	}
	if r.Notes.Valid {
		v := r.Notes.String
		p.Notes = &v
	}
	if r.Breakdown.Valid && r.Breakdown.String != "" {
		var lines []paymentDomain.BreakdownLine
		if err := json.Unmarshal([]byte(r.Breakdown.String), &lines); err == nil {
			p.Breakdown = lines
		}
	}
	return p
}

func enqueuePayment(stx *sharedDomain.SqlxTransaction, p *paymentDomain.Payment) error {
	if stx.Queue == nil {
		return nil
	}
	// All NOT NULL columns must be in the payload — the cloud projector's
	// UPSERT only emits columns present in the map, and a missing required
	// column on first-sight INSERT triggers a 23502 NOT NULL violation.
	var breakdownPayload any
	if len(p.Breakdown) > 0 {
		breakdownPayload = p.Breakdown
	}
	payload, err := json.Marshal(map[string]any{
		"id":                      p.ID.String(),
		"gym_id":                  p.GymID.String(),
		"version":                 p.Version,
		"created_at":              p.CreatedAt.UnixMilli(),
		"updated_at":              p.UpdatedAt.UnixMilli(),
		"folio":                   p.Folio,
		"member_id":               uuidPtrOrNil(p.MemberID),
		"membership_id":           uuidPtrOrNil(p.MembershipID),
		"idempotency_key":         nullableString(p.IdempotencyKey),
		"idempotency_fingerprint": nullableString(p.IdempotencyFingerprint),
		"idempotency_result":      json.RawMessage(p.IdempotencyResult),
		"amount":                  p.Amount,
		"recognized_amount":       p.RecognizedAmount,
		"payment_method":          p.PaymentMethod,
		"cash_drawer_id":          paymentCashDrawerID(p),
		"cash_destination":        p.EffectiveCashDestination(),
		"concept":                 p.Concept,
		"parent_payment_id":       uuidPtrOrNil(p.ParentPaymentID),
		"discount_amount":         p.DiscountAmount,
		"discount_reason":         strPtrOrNil(p.DiscountReason),
		"balance_pending":         p.BalancePending,
		"payment_date":            p.PaymentDate.UTC().Format(dateLayout),
		"notes":                   strPtrOrNil(p.Notes),
		"breakdown":               breakdownPayload,
		"operator_id":             p.OperatorID.String(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "payments", p.ID.String(), "upsert", payload, p.Version)
}

func paymentCashDrawerID(p *paymentDomain.Payment) any {
	if p == nil || p.PaymentMethod != paymentDomain.MethodCash || p.EffectiveCashDestination() == "gym_fund" {
		return nil
	}
	if p.CashDrawerID != nil && *p.CashDrawerID != uuid.Nil {
		return p.CashDrawerID.String()
	}
	return p.GymID.String()
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

// strPtrOrNil returns the dereferenced string for non-nil pointers and
// untyped nil otherwise. Using nil instead of "" for nullable Postgres
// columns avoids relying on the projector's empty-string nullification.
func strPtrOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// uuidPtrOrNil returns the UUID's string form for non-nil pointers and
// untyped nil otherwise — same rationale as strPtrOrNil for FK columns.
func uuidPtrOrNil(u *uuid.UUID) any {
	if u == nil {
		return nil
	}
	return u.String()
}
