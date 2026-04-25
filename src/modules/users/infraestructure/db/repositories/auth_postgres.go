//go:build server

package repositories

import (
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type PasswordResetPostgresRepository struct{}

func NewPasswordResetPostgresRepository() *PasswordResetPostgresRepository {
	return &PasswordResetPostgresRepository{}
}

func (r *PasswordResetPostgresRepository) Save(tx sharedDomain.Transaction, rec userRepo.PasswordResetRecord) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	hash := sha256.Sum256([]byte(rec.PlainToken))
	m := models.PasswordResetTokenModel{
		ID:        rec.ID,
		UserID:    rec.UserID,
		TokenHash: hash[:],
		ExpiresAt: rec.ExpiresAt,
		CreatedAt: time.Now().UTC(),
	}
	return gormTx.Create(&m).Error
}

func (r *PasswordResetPostgresRepository) Consume(tx sharedDomain.Transaction, plainToken string, now time.Time) (uuid.UUID, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	hash := sha256.Sum256([]byte(plainToken))
	var m models.PasswordResetTokenModel
	err := gormTx.Where("token_hash = ? AND used_at IS NULL", hash[:]).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return uuid.Nil, sharedDomain.NewBusinessError(userErrors.ErrInvalidResetToken, "")
	}
	if err != nil {
		return uuid.Nil, err
	}
	if m.ExpiresAt.Before(now) {
		return uuid.Nil, sharedDomain.NewBusinessError(userErrors.ErrResetTokenExpired, "")
	}
	if err := gormTx.Model(&m).Update("used_at", now).Error; err != nil {
		return uuid.Nil, err
	}
	return m.UserID, nil
}

type RefreshTokenBlacklistPostgresRepository struct{}

func NewRefreshTokenBlacklistPostgresRepository() *RefreshTokenBlacklistPostgresRepository {
	return &RefreshTokenBlacklistPostgresRepository{}
}

func (r *RefreshTokenBlacklistPostgresRepository) Revoke(tx sharedDomain.Transaction, tokenHash []byte, userID uuid.UUID, expiresAt time.Time) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := models.RefreshTokenBlacklistModel{
		TokenHash: tokenHash,
		UserID:    userID,
		ExpiresAt: expiresAt,
		RevokedAt: time.Now().UTC(),
	}
	// Idempotent: if it's already revoked, that's fine.
	return gormTx.Where("token_hash = ?", tokenHash).
		Assign(map[string]any{
			"user_id":    m.UserID,
			"expires_at": m.ExpiresAt,
			"revoked_at": m.RevokedAt,
		}).
		FirstOrCreate(&m).Error
}

func (r *RefreshTokenBlacklistPostgresRepository) IsRevoked(tx sharedDomain.Transaction, tokenHash []byte) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.RefreshTokenBlacklistModel{}).
		Where("token_hash = ?", tokenHash).
		Count(&n).Error
	return n > 0, err
}

// RevokeAllForUser is a coarse hammer used by UC-004 (password reset). We
// don't have the live tokens client-side, so we record a "revoke all before
// now" sentinel: every refresh token issued before this moment, for this user,
// is treated as revoked. The validate path checks issued_at against this row.
func (r *RefreshTokenBlacklistPostgresRepository) RevokeAllForUser(tx sharedDomain.Transaction, userID uuid.UUID, expiresAt time.Time) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	now := time.Now().UTC()
	// We use a synthetic token_hash unique per (user_id, "revoke-all") so the
	// FOR UPDATE / lookup is cheap. Stored as sha256("revoke-all:<user>").
	tag := sha256.Sum256([]byte("revoke-all:" + userID.String()))
	m := models.RefreshTokenBlacklistModel{
		TokenHash: tag[:],
		UserID:    userID,
		ExpiresAt: expiresAt,
		RevokedAt: now,
	}
	return gormTx.Where("token_hash = ?", tag[:]).
		Assign(map[string]any{"revoked_at": now, "expires_at": expiresAt}).
		FirstOrCreate(&m).Error
}
