//go:build server

package repositories

import (
	"context"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceproof"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type DeviceProofPostgresRepository struct{}

func NewDeviceProofRepository() *DeviceProofPostgresRepository {
	return &DeviceProofPostgresRepository{}
}

var _ repository.DeviceProofRepository = (*DeviceProofPostgresRepository)(nil)

func deviceProofConnection(handle shared.Transaction, actor security.AccessClaims) (*gorm.DB, error) {
	if actor.SessionID == uuid.Nil || actor.DeviceID == uuid.Nil {
		return nil, personalErrors.ErrInvalidProof
	}
	tx, err := authSessionConnection(handle, actor.AccountID, true)
	if err != nil {
		return nil, err
	}
	var valid bool
	err = tx.Raw(`SELECT coalesce(current_setting('mancuerna.attempt_id',true),'')='' AND coalesce(current_setting('mancuerna.attempt_hash',true),'')=''`).Scan(&valid).Error
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, personalErrors.ErrAuthSessionScope
	}
	err = tx.Raw("SELECT mancuerna_private.session_active(?,?)", actor.SessionID, actor.DeviceID).Scan(&valid).Error
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, personalErrors.ErrSessionRevoked
	}
	return tx, nil
}
func (*DeviceProofPostgresRepository) Create(handle shared.Transaction, actor security.AccessClaims, p *deviceproof.Proof) (err error) {
	defer func() { err = deviceProofPersistenceError(err) }()
	if p.Validate() != nil || !p.MatchesActor(actor) || p.VerifiedAt != nil || p.TokenHash != nil || p.ConsumedAt != nil || p.OperationID != nil {
		return personalErrors.ErrInvalidProof
	}
	tx, err := deviceProofConnection(handle, actor)
	if err != nil {
		return err
	}
	var valid bool
	if err = tx.Raw("SELECT ?>clock_timestamp() AND ?<=clock_timestamp()+interval '5 minutes'", p.ExpiresAt, p.ExpiresAt).Scan(&valid).Error; err != nil {
		return err
	}
	if !valid {
		return personalErrors.ErrInvalidProof
	}
	row := deviceProofToModel(p)
	return tx.Create(&row).Error
}
func (*DeviceProofPostgresRepository) GetForUpdate(handle shared.Transaction, actor security.AccessClaims, id uuid.UUID) (p *deviceproof.Proof, err error) {
	defer func() { err = deviceProofPersistenceError(err) }()
	tx, err := deviceProofConnection(handle, actor)
	if err != nil {
		return nil, err
	}
	var row models.DeviceProofModel
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id=? AND session_id=? AND device_id=? AND id=?", actor.AccountID, actor.SessionID, actor.DeviceID, id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, personalErrors.ErrInvalidProof
	}
	if err != nil {
		return nil, err
	}
	return deviceProofToDomain(row)
}
func (*DeviceProofPostgresRepository) SaveVerified(handle shared.Transaction, actor security.AccessClaims, p *deviceproof.Proof) (err error) {
	defer func() { err = deviceProofPersistenceError(err) }()
	if p.Validate() != nil || !p.MatchesActor(actor) || p.TokenHash == nil || p.VerifiedAt == nil || p.ConsumedAt != nil || p.OperationID != nil {
		return personalErrors.ErrInvalidProof
	}
	tx, err := deviceProofConnection(handle, actor)
	if err != nil {
		return err
	}
	result := tx.Model(&models.DeviceProofModel{}).Where(`account_id=? AND session_id=? AND device_id=? AND id=? AND purpose=? AND nonce=? AND request_hash=? AND expires_at=?
 AND token_hash IS NULL AND verified_at IS NULL AND consumed_at IS NULL AND operation_id IS NULL AND expires_at>clock_timestamp() AND ?<=clock_timestamp()`, actor.AccountID, actor.SessionID, actor.DeviceID, p.ID, string(p.Purpose), p.Nonce[:], p.TargetRequestHash[:], p.ExpiresAt, p.VerifiedAt).Updates(map[string]any{"token_hash": p.TokenHash[:], "verified_at": p.VerifiedAt})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return personalErrors.ErrInvalidProof
	}
	return nil
}
func (*DeviceProofPostgresRepository) Consume(handle shared.Transaction, actor security.AccessClaims, tokenHash [32]byte, purpose security.ProofPurpose, targetHash [32]byte, operationID uuid.UUID, now time.Time) (err error) {
	defer func() { err = deviceProofPersistenceError(err) }()
	if tokenHash == [32]byte{} || targetHash == [32]byte{} || operationID == uuid.Nil {
		return personalErrors.ErrInvalidProof
	}
	tx, err := deviceProofConnection(handle, actor)
	if err != nil {
		return err
	}
	var row models.DeviceProofModel
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id=? AND session_id=? AND device_id=? AND token_hash=?", actor.AccountID, actor.SessionID, actor.DeviceID, tokenHash[:]).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return personalErrors.ErrInvalidProof
	}
	if err != nil {
		return err
	}
	p, err := deviceProofToDomain(row)
	if err != nil {
		return err
	}
	if err = p.Consume(actor, purpose, targetHash, tokenHash, operationID, now); err != nil {
		return err
	}
	result := tx.Model(&models.DeviceProofModel{}).Where(`account_id=? AND session_id=? AND device_id=? AND id=? AND token_hash=? AND consumed_at IS NULL AND operation_id IS NULL
 AND expires_at>clock_timestamp() AND ?<=clock_timestamp()`, actor.AccountID, actor.SessionID, actor.DeviceID, p.ID, tokenHash[:], p.ConsumedAt).Updates(map[string]any{"consumed_at": p.ConsumedAt, "operation_id": p.OperationID})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return personalErrors.ErrInvalidProof
	}
	return nil
}
func deviceProofPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{personalErrors.ErrInvalidProof, personalErrors.ErrSessionRevoked, personalErrors.ErrAuthSessionScope, shared.ErrPersonalScope, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return personalErrors.ErrIdempotencyMismatch
	}
	return shared.NewUnexpectedError(errors.New("no se pudo consultar o guardar la prueba de dispositivo"))
}
func deviceProofToModel(p *deviceproof.Proof) models.DeviceProofModel {
	m := models.DeviceProofModel{ID: p.ID, AccountID: p.AccountID, SessionID: p.SessionID, DeviceID: p.DeviceID, Purpose: string(p.Purpose), Nonce: append([]byte(nil), p.Nonce[:]...), RequestHash: append([]byte(nil), p.TargetRequestHash[:]...), ExpiresAt: p.ExpiresAt, VerifiedAt: utcPointer(p.VerifiedAt), ConsumedAt: utcPointer(p.ConsumedAt)}
	if p.TokenHash != nil {
		m.TokenHash = append([]byte(nil), p.TokenHash[:]...)
	}
	if p.OperationID != nil {
		v := *p.OperationID
		m.OperationID = &v
	}
	return m
}
func deviceProofToDomain(m models.DeviceProofModel) (*deviceproof.Proof, error) {
	if len(m.Nonce) != 32 || len(m.RequestHash) != 32 || (len(m.TokenHash) != 0 && len(m.TokenHash) != 32) {
		return nil, personalErrors.ErrInvalidProof
	}
	p := &deviceproof.Proof{ID: m.ID, AccountID: m.AccountID, SessionID: m.SessionID, DeviceID: m.DeviceID, Purpose: security.ProofPurpose(m.Purpose), Nonce: [32]byte(m.Nonce), TargetRequestHash: [32]byte(m.RequestHash), ExpiresAt: m.ExpiresAt.UTC(), VerifiedAt: utcPointer(m.VerifiedAt), ConsumedAt: utcPointer(m.ConsumedAt)}
	if len(m.TokenHash) == 32 {
		v := [32]byte(m.TokenHash)
		p.TokenHash = &v
	}
	if m.OperationID != nil {
		v := *m.OperationID
		p.OperationID = &v
	}
	return p, p.Validate()
}
