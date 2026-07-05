//go:build sidecar

package repositories

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type TransferSQLiteRepository struct{}

func NewTransferSQLiteRepository() *TransferSQLiteRepository { return &TransferSQLiteRepository{} }

func (r *TransferSQLiteRepository) RecordTransfer(tx sharedDomain.Transaction, gymID, fromUserID, toUserID uuid.UUID) (uuid.UUID, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	id := uuid.New()
	now := time.Now().UTC()
	nowMs := now.UnixMilli()
	_, err := stx.Exec(context.Background(), `
		INSERT INTO gym_ownership_transfers (id, gym_id, version, created_at, updated_at, from_user_id, to_user_id, executed_at)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?)`,
		id.String(), gymID.String(), nowMs, nowMs, fromUserID.String(), toUserID.String(), nowMs)
	if err != nil {
		return uuid.Nil, err
	}
	if stx.Queue != nil {
		payload, _ := json.Marshal(map[string]any{
			"id":           id.String(),
			"gym_id":       gymID.String(),
			"from_user_id": fromUserID.String(),
			"to_user_id":   toUserID.String(),
			"executed_at":  nowMs,
			"created_at":   nowMs,
			"updated_at":   nowMs,
		})
		_ = stx.EnqueueSync(context.Background(), "gym_ownership_transfers", id.String(), "upsert", payload, 1)
	}
	return id, nil
}

// Sidecar OTP repo — sidecar local to keep parity. In practice the OTP flow
// for UC-010 runs cloud-side (email delivery requires cloud); the sidecar
// impl is here for tests and offline echo.
type TransferOTPSQLiteRepository struct{}

func NewTransferOTPSQLiteRepository() *TransferOTPSQLiteRepository {
	return &TransferOTPSQLiteRepository{}
}

func (r *TransferOTPSQLiteRepository) Save(tx sharedDomain.Transaction, otp gymRepo.OTPRecord) error {
	stx := tx.(*sharedDomain.SqlxTransaction)
	hash := sha256.Sum256([]byte(otp.PlainCode))
	_, err := stx.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS ownership_transfer_otps_local (
		    id TEXT PRIMARY KEY,
		    gym_id TEXT NOT NULL,
		    from_user_id TEXT NOT NULL,
		    to_user_id TEXT NOT NULL,
		    code_hash BLOB NOT NULL,
		    expires_at INTEGER NOT NULL,
		    used_at INTEGER,
		    created_at INTEGER NOT NULL
		)`)
	if err != nil {
		return err
	}
	_, err = stx.Exec(context.Background(), `
		INSERT INTO ownership_transfer_otps_local (id, gym_id, from_user_id, to_user_id, code_hash, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		otp.ID.String(), otp.GymID.String(), otp.FromUserID.String(), otp.ToUserID.String(),
		hash[:], otp.ExpiresAt, time.Now().UTC().Unix())
	return err
}

func (r *TransferOTPSQLiteRepository) Consume(tx sharedDomain.Transaction, gymID, fromUserID uuid.UUID, plainCode string) (*gymRepo.OTPRecord, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	hash := sha256.Sum256([]byte(plainCode))
	type row struct {
		ID        string `db:"id"`
		ToUserID  string `db:"to_user_id"`
		ExpiresAt int64  `db:"expires_at"`
	}
	var rec row
	err := stx.Get(context.Background(), &rec, `
		SELECT id, to_user_id, expires_at FROM ownership_transfer_otps_local
		WHERE gym_id = ? AND from_user_id = ? AND code_hash = ? AND used_at IS NULL AND expires_at > ?
		LIMIT 1`,
		gymID.String(), fromUserID.String(), hash[:], time.Now().UTC().Unix())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(userErrors.ErrInvalidOTP, "")
	}
	if err != nil {
		return nil, err
	}
	if _, err := stx.Exec(context.Background(),
		`UPDATE ownership_transfer_otps_local SET used_at = ? WHERE id = ?`,
		time.Now().UTC().Unix(), rec.ID); err != nil {
		return nil, err
	}
	id, _ := uuid.Parse(rec.ID)
	to, _ := uuid.Parse(rec.ToUserID)
	return &gymRepo.OTPRecord{
		ID:         id,
		GymID:      gymID,
		FromUserID: fromUserID,
		ToUserID:   to,
		CodeHash:   hash[:],
		ExpiresAt:  rec.ExpiresAt,
	}, nil
}
