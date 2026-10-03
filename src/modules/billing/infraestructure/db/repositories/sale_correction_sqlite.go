//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	saleDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/sale"
	correctionDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/salecorrection"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type SaleCorrectionSQLiteRepository struct{}

func NewSaleCorrectionSQLiteRepository() *SaleCorrectionSQLiteRepository {
	return &SaleCorrectionSQLiteRepository{}
}

type correctionSQLiteRow struct {
	ID                     string         `db:"id"`
	GymID                  string         `db:"gym_id"`
	SaleID                 string         `db:"sale_id"`
	Reason                 string         `db:"reason"`
	CorrectionType         string         `db:"correction_type"`
	MoneyResolution        string         `db:"money_resolution"`
	IncreaseResolution     sql.NullString `db:"increase_resolution"`
	BeforeSnapshot         string         `db:"before_snapshot"`
	AfterSnapshot          string         `db:"after_snapshot"`
	Version                int            `db:"version"`
	ExpectedSaleVersion    int            `db:"expected_sale_version"`
	CreatedAt              int64          `db:"created_at"`
	UpdatedAt              int64          `db:"updated_at"`
	DeletedAt              sql.NullInt64  `db:"deleted_at"`
	SyncedAt               sql.NullInt64  `db:"synced_at"`
	MonetaryDelta          int64          `db:"monetary_delta"`
	RefundID               sql.NullString `db:"refund_id"`
	IdempotencyKey         string         `db:"idempotency_key"`
	IdempotencyFingerprint string         `db:"idempotency_fingerprint"`
	IdempotencyResult      sql.NullString `db:"idempotency_result"`
	CreatedBy              string         `db:"created_by"`
}

func (r *SaleCorrectionSQLiteRepository) ApplySaleCorrection(tx sharedDomain.Transaction, sale *saleDomain.Sale, items []*saleDomain.SaleItem, expected int) error {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var saleDeletedAt any
	if sale.DeletedAt != nil {
		saleDeletedAt = sale.DeletedAt.UnixMilli()
	}
	res, err := stx.Exec(context.Background(), `UPDATE sales SET version=?,updated_at=?,subtotal=?,discount=?,total=?,correction_version=?,deleted_at=?
		WHERE gym_id=? AND id=? AND correction_version=? AND deleted_at IS NULL`,
		sale.Version, sale.UpdatedAt.UnixMilli(), toCents(sale.Subtotal), toCents(sale.Discount),
		toCents(sale.Total), sale.CorrectionVersion, saleDeletedAt, sale.GymID.String(), sale.ID.String(), expected)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return sharedDomain.NewBusinessError(billingErrors.ErrSaleVersionConflict, "")
	}
	if err := enqueueSale(stx, sale); err != nil {
		return err
	}
	for _, item := range items {
		row := saleItemToRow(item)
		const upsert = `INSERT INTO sale_items(id,gym_id,version,created_at,updated_at,deleted_at,sale_id,product_id,
			product_name_snapshot,unit_price_snapshot,unit_cost_snapshot,quantity,line_total)
		VALUES(:id,:gym_id,:version,:created_at,:updated_at,:deleted_at,:sale_id,:product_id,
			:product_name_snapshot,:unit_price_snapshot,:unit_cost_snapshot,:quantity,:line_total)
		ON CONFLICT(id) DO UPDATE SET version=excluded.version,updated_at=excluded.updated_at,
			deleted_at=excluded.deleted_at,product_id=excluded.product_id,
			product_name_snapshot=excluded.product_name_snapshot,unit_price_snapshot=excluded.unit_price_snapshot,
			unit_cost_snapshot=excluded.unit_cost_snapshot,quantity=excluded.quantity,line_total=excluded.line_total`
		if _, err := stx.NamedExec(context.Background(), upsert, row); err != nil {
			return err
		}
		if err := enqueueSaleItem(stx, item); err != nil {
			return err
		}
	}
	return nil
}

func (r *SaleCorrectionSQLiteRepository) CreateCorrection(tx sharedDomain.Transaction, c *correctionDomain.Correction) (*correctionDomain.Correction, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var deletedAt, refundID any
	if c.DeletedAt != nil {
		deletedAt = c.DeletedAt.UnixMilli()
	}
	if c.RefundID != nil {
		refundID = c.RefundID.String()
	}
	_, err := stx.Exec(context.Background(), `INSERT INTO sale_corrections(
		id,gym_id,version,created_at,updated_at,deleted_at,synced_at,sale_id,expected_sale_version,
		reason,correction_type,money_resolution,increase_resolution,monetary_delta,before_snapshot,after_snapshot,refund_id,idempotency_key,idempotency_fingerprint,idempotency_result,created_by)
		VALUES(?,?,?,?,?,?,NULL,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.ID.String(), c.GymID.String(), c.Version,
		c.CreatedAt.UnixMilli(), c.UpdatedAt.UnixMilli(), deletedAt, c.SaleID.String(), c.ExpectedSaleVersion,
		c.Reason, c.CorrectionType, c.MoneyResolution, nullableCorrectionSQLiteText(c.IncreaseResolution), toCents(c.MonetaryDelta), string(c.BeforeSnapshot), string(c.AfterSnapshot),
		refundID, c.IdempotencyKey, c.IdempotencyFingerprint, nullableCorrectionSQLiteJSON(c.IdempotencyResult), c.CreatedBy.String())
	if err != nil {
		return nil, err
	}
	if err := enqueueSaleCorrection(stx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *SaleCorrectionSQLiteRepository) FinalizeCorrectionIdempotency(tx sharedDomain.Transaction, c *correctionDomain.Correction) error {
	if !json.Valid(c.IdempotencyResult) {
		return errors.New("sale correction idempotency result is invalid")
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	res, err := stx.Exec(context.Background(), `UPDATE sale_corrections SET idempotency_result=?
		WHERE gym_id=? AND id=? AND deleted_at IS NULL`, string(c.IdempotencyResult), c.GymID.String(), c.ID.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return billingErrors.ErrSaleCorrectionNotFound
	}
	return enqueueSaleCorrection(stx, c)
}

func (r *SaleCorrectionSQLiteRepository) GetCorrectionByID(tx sharedDomain.Transaction, gymID, id uuid.UUID, _ bool) (*correctionDomain.Correction, error) {
	return getCorrectionSQLite(tx.(*sharedDomain.SqlxTransaction), `gym_id=? AND id=?`, gymID.String(), id.String())
}

func (r *SaleCorrectionSQLiteRepository) GetCorrectionByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*correctionDomain.Correction, error) {
	c, err := getCorrectionSQLite(tx.(*sharedDomain.SqlxTransaction), `gym_id=? AND idempotency_key=?`, gymID.String(), key)
	if errors.Is(err, billingErrors.ErrSaleCorrectionNotFound) {
		return nil, nil
	}
	return c, err
}

func getCorrectionSQLite(stx *sharedDomain.SqlxTransaction, where string, args ...any) (*correctionDomain.Correction, error) {
	var row correctionSQLiteRow
	err := stx.Get(context.Background(), &row, `SELECT id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
		sale_id,expected_sale_version,reason,correction_type,money_resolution,increase_resolution,monetary_delta,before_snapshot,after_snapshot,
		refund_id,idempotency_key,idempotency_fingerprint,idempotency_result,created_by FROM sale_corrections WHERE deleted_at IS NULL AND `+where, args...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, billingErrors.ErrSaleCorrectionNotFound
	}
	if err != nil {
		return nil, err
	}
	return correctionFromSQLite(row), nil
}

func (r *SaleCorrectionSQLiteRepository) ListCorrectionsBySale(tx sharedDomain.Transaction, gymID, saleID uuid.UUID) ([]*correctionDomain.Correction, error) {
	var rows []correctionSQLiteRow
	err := tx.(*sharedDomain.SqlxTransaction).Select(context.Background(), &rows, `SELECT id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
		sale_id,expected_sale_version,reason,correction_type,money_resolution,increase_resolution,monetary_delta,before_snapshot,after_snapshot,
		refund_id,idempotency_key,idempotency_fingerprint,idempotency_result,created_by FROM sale_corrections
		WHERE gym_id=? AND sale_id=? AND deleted_at IS NULL ORDER BY created_at,id`, gymID.String(), saleID.String())
	out := make([]*correctionDomain.Correction, len(rows))
	for i := range rows {
		out[i] = correctionFromSQLite(rows[i])
	}
	return out, err
}

func (r *SaleCorrectionSQLiteRepository) PendingAmount(tx sharedDomain.Transaction, gymID, correctionID uuid.UUID, _ bool) (float64, error) {
	var cents sql.NullInt64
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &cents, `SELECT MAX(c.monetary_delta-
		COALESCE((SELECT SUM(r.amount) FROM refunds r WHERE r.correction_id=c.id AND r.kind='overcollection_settlement' AND r.deleted_at IS NULL),0),0)
		FROM sale_corrections c WHERE c.gym_id=? AND c.id=? AND c.money_resolution='refund_pending' AND c.deleted_at IS NULL`,
		gymID.String(), correctionID.String())
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !cents.Valid) {
		return 0, billingErrors.ErrPendingRefundNotFound
	}
	return fromCents(cents.Int64), err
}

func correctionFromSQLite(x correctionSQLiteRow) *correctionDomain.Correction {
	id, _ := uuid.Parse(x.ID)
	gymID, _ := uuid.Parse(x.GymID)
	saleID, _ := uuid.Parse(x.SaleID)
	by, _ := uuid.Parse(x.CreatedBy)
	c := &correctionDomain.Correction{ID: id, GymID: gymID, Version: x.Version, SaleID: saleID,
		ExpectedSaleVersion: x.ExpectedSaleVersion, Reason: x.Reason, CorrectionType: x.CorrectionType, MoneyResolution: x.MoneyResolution,
		MonetaryDelta: fromCents(x.MonetaryDelta), BeforeSnapshot: json.RawMessage(x.BeforeSnapshot),
		AfterSnapshot: json.RawMessage(x.AfterSnapshot), IdempotencyKey: x.IdempotencyKey,
		IdempotencyFingerprint: x.IdempotencyFingerprint, CreatedBy: by,
		CreatedAt: time.UnixMilli(x.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(x.UpdatedAt).UTC()}
	if x.IdempotencyResult.Valid {
		c.IdempotencyResult = json.RawMessage(x.IdempotencyResult.String)
	}
	if x.IncreaseResolution.Valid {
		c.IncreaseResolution = x.IncreaseResolution.String
	}
	if x.RefundID.Valid {
		id, _ := uuid.Parse(x.RefundID.String)
		c.RefundID = &id
	}
	if x.DeletedAt.Valid {
		v := time.UnixMilli(x.DeletedAt.Int64).UTC()
		c.DeletedAt = &v
	}
	return c
}

func enqueueSaleCorrection(stx *sharedDomain.SqlxTransaction, c *correctionDomain.Correction) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id": c.ID.String(), "gym_id": c.GymID.String(), "version": c.Version,
		"created_at": c.CreatedAt.UnixMilli(), "updated_at": c.UpdatedAt.UnixMilli(), "deleted_at": nil,
		"sale_id": c.SaleID.String(), "expected_sale_version": c.ExpectedSaleVersion,
		"reason": c.Reason, "correction_type": c.CorrectionType, "money_resolution": c.MoneyResolution,
		"increase_resolution": c.IncreaseResolution, "monetary_delta": c.MonetaryDelta,
		"before_snapshot": c.BeforeSnapshot, "after_snapshot": c.AfterSnapshot,
		"idempotency_key": c.IdempotencyKey, "idempotency_fingerprint": c.IdempotencyFingerprint,
		"idempotency_result": c.IdempotencyResult, "created_by": c.CreatedBy.String(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "sale_corrections", c.ID.String(), "upsert", payload, c.Version)
}

func nullableCorrectionSQLiteJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func nullableCorrectionSQLiteText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
