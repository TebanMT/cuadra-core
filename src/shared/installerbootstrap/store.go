//go:build server

package installerbootstrap

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type PostgresStore struct {
	db *gorm.DB
}

func NewPostgresStore(db *gorm.DB) *PostgresStore { return &PostgresStore{db: db} }

func (s *PostgresStore) Insert(ctx context.Context, gymID, userID uuid.UUID, hash []byte, expiresAt time.Time) (Bootstrap, error) {
	id := uuid.New()
	now := time.Now().UTC()
	if err := s.db.WithContext(ctx).Exec(`
		INSERT INTO installer_bootstraps (id, token_hash, gym_id, created_by_user_id, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, hash, gymID, userID, now, expiresAt,
	).Error; err != nil {
		return Bootstrap{}, err
	}
	return Bootstrap{ID: id, GymID: gymID, UserID: userID, CreatedAt: now, ExpiresAt: expiresAt}, nil
}

// Redeem atomically marks a row as consumed and returns its identity. The
// UPDATE … RETURNING pattern means concurrent redeem attempts either both
// succeed exactly once (one wins, one returns ErrAlreadyRedeemed) or both
// see ErrNotFound.
func (s *PostgresStore) Redeem(ctx context.Context, hash []byte, now time.Time) (Bootstrap, error) {
	var rec struct {
		ID                uuid.UUID
		GymID             uuid.UUID `gorm:"column:gym_id"`
		CreatedByUserID   uuid.UUID `gorm:"column:created_by_user_id"`
		CreatedAt         time.Time `gorm:"column:created_at"`
		ExpiresAt         time.Time `gorm:"column:expires_at"`
		RedeemedAt        *time.Time `gorm:"column:redeemed_at"`
		AlreadyRedeemed   bool       `gorm:"column:already_redeemed"`
	}
	// CTE so we can distinguish "already redeemed" (row exists, redeemed_at
	// not null) from "expired/never existed" (no row matched the filter).
	res := s.db.WithContext(ctx).Raw(`
		WITH target AS (
		    SELECT id FROM installer_bootstraps
		    WHERE token_hash = ?
		      AND expires_at > ?
		      AND redeemed_at IS NULL
		    FOR UPDATE
		)
		UPDATE installer_bootstraps b
		SET redeemed_at = ?
		FROM target
		WHERE b.id = target.id
		RETURNING b.id, b.gym_id, b.created_by_user_id, b.created_at, b.expires_at, b.redeemed_at,
		          FALSE AS already_redeemed`,
		hash, now, now,
	).Scan(&rec)
	if res.Error != nil {
		return Bootstrap{}, res.Error
	}
	if res.RowsAffected == 1 {
		return Bootstrap{
			ID: rec.ID, GymID: rec.GymID, UserID: rec.CreatedByUserID,
			CreatedAt: rec.CreatedAt, ExpiresAt: rec.ExpiresAt,
			RedeemedAt: rec.RedeemedAt,
		}, nil
	}
	// Row didn't update — figure out why so we return a useful error.
	var existing struct {
		ID         uuid.UUID
		ExpiresAt  time.Time  `gorm:"column:expires_at"`
		RedeemedAt *time.Time `gorm:"column:redeemed_at"`
	}
	res2 := s.db.WithContext(ctx).Raw(`
		SELECT id, expires_at, redeemed_at FROM installer_bootstraps WHERE token_hash = ? LIMIT 1`,
		hash,
	).Scan(&existing)
	if res2.Error != nil {
		return Bootstrap{}, res2.Error
	}
	if res2.RowsAffected == 0 {
		return Bootstrap{}, ErrNotFound
	}
	if existing.RedeemedAt != nil {
		return Bootstrap{}, ErrAlreadyRedeemed
	}
	return Bootstrap{}, ErrExpired
}

// Sentinel guard so an unhandled error inside Redeem never leaks `nil` as a
// success.
var _ = errors.New
