//go:build server

package repositories

import (
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/models"
	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type TransferPostgresRepository struct{}

func NewTransferPostgresRepository() *TransferPostgresRepository {
	return &TransferPostgresRepository{}
}

func (r *TransferPostgresRepository) RecordTransfer(tx sharedDomain.Transaction, gymID, fromUserID, toUserID uuid.UUID) (uuid.UUID, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	now := time.Now().UTC()
	id := uuid.New()
	m := models.GymOwnershipTransferModel{
		ID:         id,
		GymID:      gymID,
		Version:    1,
		CreatedAt:  now,
		UpdatedAt:  now,
		FromUserID: fromUserID,
		ToUserID:   toUserID,
		ExecutedAt: now,
	}
	if err := gormTx.Create(&m).Error; err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

type TransferOTPPostgresRepository struct{}

func NewTransferOTPPostgresRepository() *TransferOTPPostgresRepository {
	return &TransferOTPPostgresRepository{}
}

func (r *TransferOTPPostgresRepository) Save(tx sharedDomain.Transaction, otp gymRepo.OTPRecord) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	hash := sha256.Sum256([]byte(otp.PlainCode))
	m := models.OwnershipTransferOTPModel{
		ID:         otp.ID,
		GymID:      otp.GymID,
		FromUserID: otp.FromUserID,
		ToUserID:   otp.ToUserID,
		CodeHash:   hash[:],
		ExpiresAt:  time.Unix(otp.ExpiresAt, 0).UTC(),
		CreatedAt:  time.Now().UTC(),
	}
	return gormTx.Create(&m).Error
}

// Consume validates a candidate code: matches the hash, not yet used, not
// expired. Marks used_at on success.
func (r *TransferOTPPostgresRepository) Consume(tx sharedDomain.Transaction, gymID, fromUserID uuid.UUID, plainCode string) (*gymRepo.OTPRecord, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	hash := sha256.Sum256([]byte(plainCode))
	var m models.OwnershipTransferOTPModel
	err := gormTx.Where(
		"gym_id = ? AND from_user_id = ? AND code_hash = ? AND used_at IS NULL AND expires_at > NOW()",
		gymID, fromUserID, hash[:],
	).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(userErrors.ErrInvalidOTP, "")
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if err := gormTx.Model(&m).Update("used_at", now).Error; err != nil {
		return nil, err
	}
	return &gymRepo.OTPRecord{
		ID:         m.ID,
		GymID:      m.GymID,
		FromUserID: m.FromUserID,
		ToUserID:   m.ToUserID,
		CodeHash:   m.CodeHash,
		ExpiresAt:  m.ExpiresAt.Unix(),
	}, nil
}
