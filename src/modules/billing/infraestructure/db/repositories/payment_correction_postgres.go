//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	correctionDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/paymentcorrection"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type PaymentCorrectionPostgresRepository struct{}

func NewPaymentCorrectionPostgresRepository() *PaymentCorrectionPostgresRepository {
	return &PaymentCorrectionPostgresRepository{}
}

type paymentCorrectionPGRow struct {
	ID, GymID, PaymentID, CreatedBy uuid.UUID
	Version                         int
	ExpectedPaymentVersion          int
	Reason                          string
	BeforeSnapshot                  []byte
	AfterSnapshot                   []byte
	IdempotencyKey                  string
	IdempotencyFingerprint          string
	IdempotencyResult               []byte
	CreatedAt, UpdatedAt            time.Time
	DeletedAt                       *time.Time
}

func (paymentCorrectionPGRow) TableName() string { return "payment_corrections" }

func (r *PaymentCorrectionPostgresRepository) Create(tx sharedDomain.Transaction, c *correctionDomain.Correction) (*correctionDomain.Correction, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	err := g.Exec(`INSERT INTO payment_corrections(id,gym_id,version,created_at,updated_at,deleted_at,payment_id,
		expected_payment_version,reason,before_snapshot,after_snapshot,idempotency_key,idempotency_fingerprint,idempotency_result,created_by)
		VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,?::jsonb,?,?,?::jsonb,?)`, c.ID, c.GymID, c.Version, c.CreatedAt, c.UpdatedAt,
		c.DeletedAt, c.PaymentID, c.ExpectedPaymentVersion, c.Reason, string(c.BeforeSnapshot), string(c.AfterSnapshot),
		c.IdempotencyKey, c.IdempotencyFingerprint, nullableCorrectionJSON(c.IdempotencyResult), c.CreatedBy).Error
	if err != nil {
		return nil, err
	}
	if err := mirrorPaymentCorrection(g, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *PaymentCorrectionPostgresRepository) FinalizeIdempotency(tx sharedDomain.Transaction, c *correctionDomain.Correction) error {
	if !json.Valid(c.IdempotencyResult) {
		return errors.New("payment correction idempotency result is invalid")
	}
	g := tx.(*sharedDomain.GormTransaction).Tx
	res := g.Model(&paymentCorrectionPGRow{}).Where("gym_id=? AND id=? AND deleted_at IS NULL", c.GymID, c.ID).
		Update("idempotency_result", string(c.IdempotencyResult))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return billingErrors.ErrPaymentCorrectionNotFound
	}
	return mirrorPaymentCorrection(g, c)
}

func (r *PaymentCorrectionPostgresRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*correctionDomain.Correction, error) {
	var row paymentCorrectionPGRow
	err := tx.(*sharedDomain.GormTransaction).Tx.Where("gym_id=? AND idempotency_key=? AND deleted_at IS NULL", gymID, key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return paymentCorrectionFromPG(row), nil
}

func (r *PaymentCorrectionPostgresRepository) ListByPayment(tx sharedDomain.Transaction, gymID, paymentID uuid.UUID) ([]*correctionDomain.Correction, error) {
	var rows []paymentCorrectionPGRow
	err := tx.(*sharedDomain.GormTransaction).Tx.Where("gym_id=? AND payment_id=? AND deleted_at IS NULL", gymID, paymentID).
		Order("created_at DESC,id DESC").Find(&rows).Error
	out := make([]*correctionDomain.Correction, len(rows))
	for i := range rows {
		out[i] = paymentCorrectionFromPG(rows[i])
	}
	return out, err
}

func paymentCorrectionFromPG(x paymentCorrectionPGRow) *correctionDomain.Correction {
	return &correctionDomain.Correction{ID: x.ID, GymID: x.GymID, PaymentID: x.PaymentID, CreatedBy: x.CreatedBy,
		Version: x.Version, ExpectedPaymentVersion: x.ExpectedPaymentVersion, Reason: x.Reason,
		BeforeSnapshot: json.RawMessage(x.BeforeSnapshot), AfterSnapshot: json.RawMessage(x.AfterSnapshot),
		IdempotencyKey: x.IdempotencyKey, IdempotencyFingerprint: x.IdempotencyFingerprint,
		IdempotencyResult: json.RawMessage(x.IdempotencyResult), CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt, DeletedAt: x.DeletedAt}
}

func mirrorPaymentCorrection(g *gorm.DB, c *correctionDomain.Correction) error {
	payload, _ := json.Marshal(map[string]any{
		"id": c.ID, "gym_id": c.GymID, "version": c.Version, "created_at": c.CreatedAt.UnixMilli(),
		"updated_at": c.UpdatedAt.UnixMilli(), "deleted_at": c.DeletedAt, "payment_id": c.PaymentID,
		"expected_payment_version": c.ExpectedPaymentVersion, "reason": c.Reason,
		"before_snapshot": c.BeforeSnapshot, "after_snapshot": c.AfterSnapshot,
		"idempotency_key": c.IdempotencyKey, "idempotency_fingerprint": c.IdempotencyFingerprint,
		"idempotency_result": c.IdempotencyResult, "created_by": c.CreatedBy,
	})
	return upsertFinancialMirror(g, c.GymID, "payment_corrections", c.ID, c.Version, payload, c.DeletedAt)
}
