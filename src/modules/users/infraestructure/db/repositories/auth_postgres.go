//go:build server

package repositories

import (
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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

type EmailVerificationPostgresRepository struct{}

func NewEmailVerificationPostgresRepository() *EmailVerificationPostgresRepository {
	return &EmailVerificationPostgresRepository{}
}

func (r *EmailVerificationPostgresRepository) Save(tx sharedDomain.Transaction, rec userRepo.EmailVerificationRecord) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	hash := sha256.Sum256([]byte(rec.PlainToken))
	m := models.EmailVerificationTokenModel{
		ID:        rec.ID,
		UserID:    rec.UserID,
		TokenHash: hash[:],
		ExpiresAt: rec.ExpiresAt,
		CreatedAt: time.Now().UTC(),
	}
	return gormTx.Create(&m).Error
}

func (r *EmailVerificationPostgresRepository) Consume(tx sharedDomain.Transaction, plainToken string, now time.Time) (uuid.UUID, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	hash := sha256.Sum256([]byte(plainToken))
	var m models.EmailVerificationTokenModel
	err := gormTx.Where("token_hash = ? AND used_at IS NULL", hash[:]).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return uuid.Nil, sharedDomain.NewBusinessError(userErrors.ErrInvalidVerifyToken, "")
	}
	if err != nil {
		return uuid.Nil, err
	}
	if m.ExpiresAt.Before(now) {
		return uuid.Nil, sharedDomain.NewBusinessError(userErrors.ErrVerifyTokenExpired, "")
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

// Revoke takes effect now. Idempotent: an already revoked token stays revoked.
func (r *RefreshTokenBlacklistPostgresRepository) Revoke(tx sharedDomain.Transaction, tokenHash []byte, userID uuid.UUID, expiresAt time.Time) error {
	return r.RevokeAt(tx, tokenHash, userID, expiresAt, time.Now().UTC())
}

// RevokeAt schedules the revocation for a moment that can be in the future
// (refresh rotation leaves a short window). It never pushes an earlier
// revocation later, so a logout or a second rotation keeps the first
// deadline. ON CONFLICT also keeps two concurrent rotations of the same token
// from failing on the primary key.
func (r *RefreshTokenBlacklistPostgresRepository) RevokeAt(tx sharedDomain.Transaction, tokenHash []byte, userID uuid.UUID, expiresAt, revokedAt time.Time) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := models.RefreshTokenBlacklistModel{
		TokenHash: tokenHash,
		UserID:    userID,
		ExpiresAt: expiresAt.UTC(),
		RevokedAt: revokedAt.UTC(),
	}
	return gormTx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "token_hash"}},
		DoUpdates: clause.Set{
			{Column: clause.Column{Name: "revoked_at"}, Value: gorm.Expr("LEAST(refresh_token_blacklist.revoked_at, EXCLUDED.revoked_at)")},
			{Column: clause.Column{Name: "expires_at"}, Value: gorm.Expr("GREATEST(refresh_token_blacklist.expires_at, EXCLUDED.expires_at)")},
		},
	}).Create(&m).Error
}

// IsRevoked is true once the revocation moment has passed. A rotated token
// stays usable until its RevokeAt deadline.
func (r *RefreshTokenBlacklistPostgresRepository) IsRevoked(tx sharedDomain.Transaction, tokenHash []byte) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.RefreshTokenBlacklistModel{}).
		Where("token_hash = ? AND revoked_at <= ?", tokenHash, time.Now().UTC()).
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

// RevokedAllAt reads the sentinel RevokeAllForUser writes. An expired
// sentinel no longer matters: every refresh token it covered expired too.
func (r *RefreshTokenBlacklistPostgresRepository) RevokedAllAt(tx sharedDomain.Transaction, userID uuid.UUID) (*time.Time, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	tag := sha256.Sum256([]byte("revoke-all:" + userID.String()))
	var rows []models.RefreshTokenBlacklistModel
	if err := gormTx.Where("token_hash = ? AND expires_at > ?", tag[:], time.Now().UTC()).
		Limit(1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	at := rows[0].RevokedAt
	return &at, nil
}

func (r *RefreshTokenBlacklistPostgresRepository) DeleteExpiredForUser(tx sharedDomain.Transaction, userID uuid.UUID, now time.Time) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	return gormTx.Where("user_id = ? AND expires_at <= ?", userID, now.UTC()).
		Delete(&models.RefreshTokenBlacklistModel{}).Error
}
