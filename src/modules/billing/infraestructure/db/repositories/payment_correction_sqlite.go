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
	correctionDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/paymentcorrection"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type PaymentCorrectionSQLiteRepository struct{}

func NewPaymentCorrectionSQLiteRepository() *PaymentCorrectionSQLiteRepository {
	return &PaymentCorrectionSQLiteRepository{}
}

type paymentCorrectionSQLiteRow struct {
	ID                     string         `db:"id"`
	GymID                  string         `db:"gym_id"`
	PaymentID              string         `db:"payment_id"`
	CreatedBy              string         `db:"created_by"`
	Version                int            `db:"version"`
	ExpectedPaymentVersion int            `db:"expected_payment_version"`
	Reason                 string         `db:"reason"`
	BeforeSnapshot         string         `db:"before_snapshot"`
	AfterSnapshot          string         `db:"after_snapshot"`
	IdempotencyKey         string         `db:"idempotency_key"`
	IdempotencyFingerprint string         `db:"idempotency_fingerprint"`
	IdempotencyResult      sql.NullString `db:"idempotency_result"`
	CreatedAt              int64          `db:"created_at"`
	UpdatedAt              int64          `db:"updated_at"`
	DeletedAt              sql.NullInt64  `db:"deleted_at"`
}

func (r *PaymentCorrectionSQLiteRepository) Create(tx sharedDomain.Transaction, c *correctionDomain.Correction) (*correctionDomain.Correction, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	_, err := stx.Exec(context.Background(), `INSERT INTO payment_corrections(id,gym_id,version,created_at,updated_at,deleted_at,
		payment_id,expected_payment_version,reason,before_snapshot,after_snapshot,idempotency_key,idempotency_fingerprint,idempotency_result,created_by)
		VALUES(?,?,?,?,?,NULL,?,?,?,?,?,?,?,?,?)`, c.ID.String(), c.GymID.String(), c.Version, c.CreatedAt.UnixMilli(), c.UpdatedAt.UnixMilli(),
		c.PaymentID.String(), c.ExpectedPaymentVersion, c.Reason, string(c.BeforeSnapshot), string(c.AfterSnapshot),
		c.IdempotencyKey, c.IdempotencyFingerprint, nullableCorrectionSQLiteJSON(c.IdempotencyResult), c.CreatedBy.String())
	if err != nil {
		return nil, err
	}
	if err := enqueuePaymentCorrection(stx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *PaymentCorrectionSQLiteRepository) FinalizeIdempotency(tx sharedDomain.Transaction, c *correctionDomain.Correction) error {
	if !json.Valid(c.IdempotencyResult) {
		return errors.New("payment correction idempotency result is invalid")
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	res, err := stx.Exec(context.Background(), `UPDATE payment_corrections SET idempotency_result=? WHERE gym_id=? AND id=? AND deleted_at IS NULL`,
		string(c.IdempotencyResult), c.GymID.String(), c.ID.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return billingErrors.ErrPaymentCorrectionNotFound
	}
	return enqueuePaymentCorrection(stx, c)
}

func (r *PaymentCorrectionSQLiteRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*correctionDomain.Correction, error) {
	var row paymentCorrectionSQLiteRow
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &row, `SELECT id,gym_id,version,created_at,updated_at,deleted_at,
		payment_id,expected_payment_version,reason,before_snapshot,after_snapshot,idempotency_key,idempotency_fingerprint,idempotency_result,created_by
		FROM payment_corrections WHERE gym_id=? AND idempotency_key=? AND deleted_at IS NULL`, gymID.String(), key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return paymentCorrectionFromSQLite(row), nil
}

func (r *PaymentCorrectionSQLiteRepository) ListByPayment(tx sharedDomain.Transaction, gymID, paymentID uuid.UUID) ([]*correctionDomain.Correction, error) {
	var rows []paymentCorrectionSQLiteRow
	err := tx.(*sharedDomain.SqlxTransaction).Select(context.Background(), &rows, `SELECT id,gym_id,version,created_at,updated_at,deleted_at,
		payment_id,expected_payment_version,reason,before_snapshot,after_snapshot,idempotency_key,idempotency_fingerprint,idempotency_result,created_by
		FROM payment_corrections WHERE gym_id=? AND payment_id=? AND deleted_at IS NULL ORDER BY created_at DESC,id DESC`, gymID.String(), paymentID.String())
	out := make([]*correctionDomain.Correction, len(rows))
	for i := range rows {
		out[i] = paymentCorrectionFromSQLite(rows[i])
	}
	return out, err
}

func paymentCorrectionFromSQLite(x paymentCorrectionSQLiteRow) *correctionDomain.Correction {
	id, _ := uuid.Parse(x.ID)
	gymID, _ := uuid.Parse(x.GymID)
	paymentID, _ := uuid.Parse(x.PaymentID)
	createdBy, _ := uuid.Parse(x.CreatedBy)
	out := &correctionDomain.Correction{ID: id, GymID: gymID, PaymentID: paymentID, CreatedBy: createdBy,
		Version: x.Version, ExpectedPaymentVersion: x.ExpectedPaymentVersion, Reason: x.Reason,
		BeforeSnapshot: json.RawMessage(x.BeforeSnapshot), AfterSnapshot: json.RawMessage(x.AfterSnapshot),
		IdempotencyKey: x.IdempotencyKey, IdempotencyFingerprint: x.IdempotencyFingerprint,
		CreatedAt: time.UnixMilli(x.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(x.UpdatedAt).UTC()}
	if x.IdempotencyResult.Valid {
		out.IdempotencyResult = json.RawMessage(x.IdempotencyResult.String)
	}
	if x.DeletedAt.Valid {
		v := time.UnixMilli(x.DeletedAt.Int64).UTC()
		out.DeletedAt = &v
	}
	return out
}

func enqueuePaymentCorrection(stx *sharedDomain.SqlxTransaction, c *correctionDomain.Correction) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id": c.ID.String(), "gym_id": c.GymID.String(), "version": c.Version,
		"created_at": c.CreatedAt.UnixMilli(), "updated_at": c.UpdatedAt.UnixMilli(), "deleted_at": nil,
		"payment_id": c.PaymentID.String(), "expected_payment_version": c.ExpectedPaymentVersion, "reason": c.Reason,
		"before_snapshot": c.BeforeSnapshot, "after_snapshot": c.AfterSnapshot,
		"idempotency_key": c.IdempotencyKey, "idempotency_fingerprint": c.IdempotencyFingerprint,
		"idempotency_result": c.IdempotencyResult, "created_by": c.CreatedBy.String(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "payment_corrections", c.ID.String(), "upsert", payload, c.Version)
}
