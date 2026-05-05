//go:build server

package sidecartoken

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// PostgresStore is the production implementation. The methods take a raw
// *gorm.DB rather than a Transaction so the middleware can use it without
// dragging in the UnitOfWork — the cron + bootstrap paths inject one
// explicitly when they want write atomicity.
type PostgresStore struct {
	db *gorm.DB
}

func NewPostgresStore(db *gorm.DB) *PostgresStore { return &PostgresStore{db: db} }

func (s *PostgresStore) LookupActiveByHash(ctx context.Context, hash []byte) (Credential, error) {
	var rec struct {
		ID          uuid.UUID
		GymID       uuid.UUID `gorm:"column:gym_id"`
		ClientID    uuid.UUID `gorm:"column:client_id"`
		UserID      uuid.UUID `gorm:"column:user_id"`
		DeviceLabel *string   `gorm:"column:device_label"`
		CreatedAt   time.Time `gorm:"column:created_at"`
		LastSeenAt  time.Time `gorm:"column:last_seen_at"`
	}
	res := s.db.WithContext(ctx).Raw(`
		SELECT id, gym_id, client_id, user_id, device_label, created_at, last_seen_at
		FROM sidecar_credentials
		WHERE key_hash = ? AND revoked_at IS NULL
		LIMIT 1`, hash).Scan(&rec)
	if res.Error != nil {
		return Credential{}, res.Error
	}
	if res.RowsAffected == 0 {
		return Credential{}, ErrNotFound
	}
	return Credential{
		ID: rec.ID, GymID: rec.GymID, ClientID: rec.ClientID, UserID: rec.UserID,
		DeviceLabel: derefString(rec.DeviceLabel),
		CreatedAt:   rec.CreatedAt, LastSeenAt: rec.LastSeenAt,
	}, nil
}

func (s *PostgresStore) TouchLastSeen(ctx context.Context, credentialID uuid.UUID) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE sidecar_credentials SET last_seen_at = NOW() WHERE id = ?`,
		credentialID,
	).Error
}

func (s *PostgresStore) FindActive(ctx context.Context, gymID, clientID uuid.UUID) (Credential, error) {
	var rec struct {
		ID          uuid.UUID
		GymID       uuid.UUID `gorm:"column:gym_id"`
		ClientID    uuid.UUID `gorm:"column:client_id"`
		UserID      uuid.UUID `gorm:"column:user_id"`
		DeviceLabel *string   `gorm:"column:device_label"`
		CreatedAt   time.Time `gorm:"column:created_at"`
		LastSeenAt  time.Time `gorm:"column:last_seen_at"`
	}
	res := s.db.WithContext(ctx).Raw(`
		SELECT id, gym_id, client_id, user_id, device_label, created_at, last_seen_at
		FROM sidecar_credentials
		WHERE gym_id = ? AND client_id = ? AND revoked_at IS NULL
		LIMIT 1`, gymID, clientID).Scan(&rec)
	if res.Error != nil {
		return Credential{}, res.Error
	}
	if res.RowsAffected == 0 {
		return Credential{}, ErrNotFound
	}
	return Credential{
		ID: rec.ID, GymID: rec.GymID, ClientID: rec.ClientID, UserID: rec.UserID,
		DeviceLabel: derefString(rec.DeviceLabel),
		CreatedAt:   rec.CreatedAt, LastSeenAt: rec.LastSeenAt,
	}, nil
}

func (s *PostgresStore) Insert(ctx context.Context, gymID, clientID, userID uuid.UUID, hash []byte, deviceLabel string) (Credential, error) {
	id := uuid.New()
	now := time.Now().UTC()
	var label any
	if deviceLabel != "" {
		label = deviceLabel
	}
	if err := s.db.WithContext(ctx).Exec(`
		INSERT INTO sidecar_credentials
			(id, gym_id, client_id, user_id, key_hash, device_label, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, gymID, clientID, userID, hash, label, now, now,
	).Error; err != nil {
		return Credential{}, err
	}
	return Credential{
		ID: id, GymID: gymID, ClientID: clientID, UserID: userID,
		DeviceLabel: deviceLabel, CreatedAt: now, LastSeenAt: now,
	}, nil
}

func (s *PostgresStore) RevokeActive(ctx context.Context, gymID, clientID uuid.UUID) error {
	return s.db.WithContext(ctx).Exec(`
		UPDATE sidecar_credentials
		SET revoked_at = NOW()
		WHERE gym_id = ? AND client_id = ? AND revoked_at IS NULL`,
		gymID, clientID,
	).Error
}

func (s *PostgresStore) RevokeIdle(ctx context.Context, threshold time.Time) (int64, error) {
	res := s.db.WithContext(ctx).Exec(`
		UPDATE sidecar_credentials
		SET revoked_at = NOW()
		WHERE revoked_at IS NULL AND last_seen_at < ?`,
		threshold,
	)
	return res.RowsAffected, res.Error
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
