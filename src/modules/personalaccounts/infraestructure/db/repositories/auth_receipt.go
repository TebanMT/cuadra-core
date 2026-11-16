//go:build server

package repositories

import (
	"errors"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type AuthReceiptPostgresRepository struct{}

func NewAuthReceiptPostgresRepository() *AuthReceiptPostgresRepository {
	return &AuthReceiptPostgresRepository{}
}

var _ repository.AuthReceiptRepository = (*AuthReceiptPostgresRepository)(nil)

func receiptConnection(handle shared.Transaction) (*gorm.DB, error) {
	t, ok := handle.(*shared.GormTransaction)
	if !ok || t == nil || t.Tx == nil || t.Tx.Statement == nil {
		return nil, personalErrors.ErrInvalidReceipt
	}
	if _, ok := t.Tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return nil, personalErrors.ErrInvalidReceipt
	}
	// Error and slow-query diagnostics can interpolate response ciphertext,
	// hashes and key versions. Do not let the pool's logger render these rows.
	return t.Tx.Session(&gorm.Session{Logger: logger.Discard}), nil
}
func receiptOwner(q *gorm.DB, owner receipt.Owner) *gorm.DB {
	if owner.AccountID == uuid.Nil {
		q = q.Where("account_id IS NULL")
	} else {
		q = q.Where("account_id=?", owner.AccountID)
	}
	if owner.AttemptID == uuid.Nil {
		q = q.Where("attempt_id IS NULL")
	} else {
		q = q.Where("attempt_id=?", owner.AttemptID)
	}
	return q
}
func (*AuthReceiptPostgresRepository) Get(handle shared.Transaction, owner receipt.Owner, id uuid.UUID) (*receipt.Receipt, error) {
	if !owner.Valid() || id == uuid.Nil {
		return nil, personalErrors.ErrReceiptNotFound
	}
	tx, err := receiptConnection(handle)
	if err != nil {
		return nil, err
	}
	var m models.AuthReceiptModel
	err = receiptOwner(tx, owner).Where("operation_id=?", id).Take(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, personalErrors.ErrReceiptNotFound
	}
	if err != nil {
		return nil, receiptStorageError(err)
	}
	return receiptToDomain(m)
}
func (*AuthReceiptPostgresRepository) Create(handle shared.Transaction, r *receipt.Receipt) error {
	if err := r.Validate(); err != nil {
		return err
	}
	tx, err := receiptConnection(handle)
	if err != nil {
		return err
	}
	m := receiptToModel(r)
	if err = tx.Create(&m).Error; err != nil {
		return receiptStorageError(err)
	}
	return nil
}
func (*AuthReceiptPostgresRepository) SaveResponse(handle shared.Transaction, r *receipt.Receipt) error {
	if err := r.Validate(); err != nil {
		return err
	}
	tx, err := receiptConnection(handle)
	if err != nil {
		return err
	}
	m := receiptToModel(r)
	result := receiptOwner(tx.Model(&models.AuthReceiptModel{}), r.Owner).
		Where("operation_id=? AND purpose=? AND request_hash=? AND completed_at=?", r.OperationID, r.Purpose, r.RequestHash[:], r.CompletedAt).
		Updates(map[string]any{"response_ciphertext": m.ResponseCiphertext, "key_version": m.KeyVersion, "response_expires_at": m.ResponseExpiresAt, "result_reference": m.ResultReference})
	if result.Error != nil {
		return receiptStorageError(result.Error)
	}
	if result.RowsAffected != 1 {
		return personalErrors.ErrReceiptNotFound
	}
	return nil
}

func receiptStorageError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return personalErrors.ErrIdempotencyMismatch
	}
	// PostgreSQL Detail can contain the entire failing row. Preserve only the
	// safe idempotency classification above; never wrap a storage diagnostic.
	return shared.NewUnexpectedError(errors.New("no se pudo acceder al recibo de acceso"))
}
func receiptUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
func receiptToModel(r *receipt.Receipt) models.AuthReceiptModel {
	m := models.AuthReceiptModel{OperationID: r.OperationID, AccountID: receiptUUID(r.Owner.AccountID), AttemptID: receiptUUID(r.Owner.AttemptID), Purpose: r.Purpose, RequestHash: append([]byte(nil), r.RequestHash[:]...), ResultReference: receiptUUID(r.ResultReference), ResponseCiphertext: append([]byte(nil), r.Response.Ciphertext...), ResponseExpiresAt: r.ResponseExpiresAt.UTC(), ResultSecretHash: append([]byte(nil), r.ResultSecretHash...), CompletedAt: r.CompletedAt.UTC()}
	if r.Response.KeyVersion != "" {
		v := r.Response.KeyVersion
		m.KeyVersion = &v
	}
	return m
}
func receiptToDomain(m models.AuthReceiptModel) (*receipt.Receipt, error) {
	if len(m.RequestHash) != 32 {
		return nil, personalErrors.ErrInvalidReceipt
	}
	r := &receipt.Receipt{OperationID: m.OperationID, Purpose: m.Purpose, Response: security.Envelope{Ciphertext: append([]byte(nil), m.ResponseCiphertext...)}, ResponseExpiresAt: m.ResponseExpiresAt.UTC(), ResultSecretHash: append([]byte(nil), m.ResultSecretHash...), CompletedAt: m.CompletedAt.UTC()}
	copy(r.RequestHash[:], m.RequestHash)
	if m.AccountID != nil {
		r.Owner.AccountID = *m.AccountID
	}
	if m.AttemptID != nil {
		r.Owner.AttemptID = *m.AttemptID
	}
	if m.ResultReference != nil {
		r.ResultReference = *m.ResultReference
	}
	if m.KeyVersion != nil {
		r.Response.KeyVersion = *m.KeyVersion
	}
	return r, r.Validate()
}
