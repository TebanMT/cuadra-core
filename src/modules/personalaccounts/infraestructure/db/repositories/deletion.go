//go:build server

package repositories

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deletion"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type DeletionPostgresRepository struct{}

func NewDeletionRepository() *DeletionPostgresRepository { return &DeletionPostgresRepository{} }

var _ repository.DeletionRepository = (*DeletionPostgresRepository)(nil)

func deletionConnection(h shared.Transaction, accountID uuid.UUID, write bool) (*gorm.DB, error) {
	tx, err := authSessionConnection(h, accountID, write)
	if err != nil {
		return nil, err
	}
	var pure bool
	err = tx.Raw(`SELECT coalesce(current_setting('mancuerna.attempt_id',true),'')='' AND coalesce(current_setting('mancuerna.attempt_hash',true),'')=''`).Scan(&pure).Error
	if err != nil {
		return nil, err
	}
	if !pure {
		return nil, shared.ErrPersonalScope
	}
	return tx.Session(&gorm.Session{Logger: logger.Discard}), nil
}

func (*DeletionPostgresRepository) Request(h shared.Transaction, actor security.AccessClaims, request deletion.Request) (err error) {
	defer func() { err = deletionPersistenceError(err) }()
	if err = request.Validate(); err != nil {
		return err
	}
	if actor.SessionID == uuid.Nil || actor.DeviceID == uuid.Nil {
		return personalErrors.ErrInvalidDeletion
	}
	tx, err := deletionConnection(h, actor.AccountID, true)
	if err != nil {
		return err
	}
	var active bool
	if err = tx.Raw(`SELECT mancuerna_private.session_active(?,?)`, actor.SessionID, actor.DeviceID).Scan(&active).Error; err != nil {
		return err
	}
	if !active {
		return personalErrors.ErrSessionRevoked
	}
	// The effect cannot be called with only an account identifier. Require its
	// exact proof consumption inside the current account transaction.
	var proof bool
	err = tx.Raw(`SELECT EXISTS(SELECT 1 FROM mancuerna.device_proofs WHERE account_id=? AND session_id=? AND device_id=?
 AND purpose='delete_account' AND token_hash=? AND request_hash=? AND operation_id=? AND consumed_at=?
 AND expires_at>clock_timestamp() AND ?<=clock_timestamp())`, actor.AccountID, actor.SessionID, actor.DeviceID, request.ProofHash[:], request.RequestHash[:], request.OperationID, request.RequestedAt, request.RequestedAt).Scan(&proof).Error
	if err != nil {
		return err
	}
	if !proof {
		return personalErrors.ErrInvalidProof
	}
	var row models.DeletionAccountModel
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", actor.AccountID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return personalErrors.ErrDeletionState
	}
	if err != nil {
		return err
	}
	state := deletionAccountToDomain(row)
	previousVersion := state.SecurityVersion
	if err = state.Request(&request.Policy, request.RequestedAt); err != nil {
		return err
	}
	result := tx.Model(&models.DeletionAccountModel{}).Where("id=? AND status='active' AND deleted_at IS NULL AND security_version=?", actor.AccountID, previousVersion).
		Updates(map[string]any{"status": state.Status, "security_version": state.SecurityVersion, "deletion_requested_at": state.RequestedAt, "deletion_policy_version": state.PolicyVersion, "purge_after": state.PurgeAfter, "updated_at": state.RequestedAt})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return personalErrors.ErrDeletionState
	}
	if err = tx.Exec(`UPDATE mancuerna.sessions SET revoked_at=? WHERE account_id=? AND revoked_at IS NULL`, state.RequestedAt, actor.AccountID).Error; err != nil {
		return err
	}
	// Account-bound attempts are the only ones visible under this RLS scope.
	// Unbound attempts remain unable to authorize an inactive account later.
	err = tx.Exec(`WITH cancelled AS (
 UPDATE mancuerna.auth_challenges SET status='cancelled',revision=revision+1
 WHERE account_id=? AND status IN ('pending','uncertain','verified') RETURNING id
 ) UPDATE mancuerna.device_authorizations SET status='cancelled',decided_at=?
 WHERE account_id=? AND challenge_id IN (SELECT id FROM cancelled) AND status IN ('pending','approved')`, actor.AccountID, state.RequestedAt, actor.AccountID).Error
	return err
}

func (*DeletionPostgresRepository) GetResult(h shared.Transaction, accountID, operationID uuid.UUID, hash [32]byte) (result deletion.Result, err error) {
	defer func() {
		err = deletionPersistenceError(err)
		if err != nil {
			result = deletion.Result{}
		}
	}()
	if operationID == uuid.Nil || hash == [32]byte{} {
		return result, personalErrors.ErrDeletionResultNotFound
	}
	tx, err := deletionConnection(h, accountID, false)
	if err != nil {
		return result, err
	}
	// Deliberately select no ciphertext, session reference, phone or profile.
	// A resolver match is not enough: the exact receipt and accepted account
	// transition must agree before granting this minimal response capability.
	var row struct {
		OperationID      uuid.UUID
		CompletedAt      time.Time
		ResultSecretHash []byte
	}
	err = tx.Raw(`SELECT r.operation_id,r.completed_at,r.result_secret_hash FROM mancuerna.auth_receipts r
 JOIN mancuerna.accounts a ON a.id=r.account_id
 WHERE r.account_id=? AND r.operation_id=? AND r.attempt_id IS NULL AND r.purpose='delete_account' AND r.result_secret_hash=?
 AND r.response_ciphertext IS NULL AND r.key_version IS NULL
 AND a.status IN ('deletion_pending','deleted') AND a.deletion_requested_at=r.completed_at
 AND a.deletion_policy_version IS NOT NULL AND a.deletion_policy_version<>'' AND a.purge_after>a.deletion_requested_at`, accountID, operationID, hash[:]).Scan(&row).Error
	if err != nil {
		return result, err
	}
	if row.OperationID != operationID || subtle.ConstantTimeCompare(row.ResultSecretHash, hash[:]) != 1 {
		return result, personalErrors.ErrDeletionResultNotFound
	}
	result = deletion.Result{OperationID: row.OperationID, CompletedAt: row.CompletedAt.UTC()}
	return result, result.Validate()
}

func deletionAccountToDomain(row models.DeletionAccountModel) *deletion.State {
	s := &deletion.State{AccountID: row.ID, Status: row.Status, SecurityVersion: row.SecurityVersion, RequestedAt: utcPointer(row.RequestedAt), PurgeAfter: utcPointer(row.PurgeAfter), DeletedAt: utcPointer(row.DeletedAt)}
	if row.PolicyVersion != nil {
		s.PolicyVersion = *row.PolicyVersion
	}
	return s
}

type DeletionResultPostgresResolver struct{ db *gorm.DB }

func NewDeletionResultResolver(db *gorm.DB) (*DeletionResultPostgresResolver, error) {
	if _, err := shared.NewPersonalPostgresUnitOfWork(db, shared.PersonalAuthRole); err != nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &DeletionResultPostgresResolver{db: db.Session(&gorm.Session{Logger: logger.Discard})}, nil
}

var _ repository.DeletionResultResolver = (*DeletionResultPostgresResolver)(nil)

func (r *DeletionResultPostgresResolver) Resolve(ctx context.Context, hash [32]byte) (id uuid.UUID, err error) {
	defer func() {
		err = deletionPersistenceError(err)
		if err != nil {
			id = uuid.Nil
		}
	}()
	if err = shared.ValidatePersonalAnonymousScope(ctx); err != nil {
		return uuid.Nil, err
	}
	if r == nil || r.db == nil || hash == [32]byte{} {
		return uuid.Nil, personalErrors.ErrDeletionResultNotFound
	}
	var result struct{ AccountID *uuid.UUID }
	err = r.db.WithContext(ctx).Raw(`SELECT mancuerna_private.resolve_identity('deletion_result',?) AS account_id`, hex.EncodeToString(hash[:])).Scan(&result).Error
	if err != nil {
		return uuid.Nil, err
	}
	if result.AccountID == nil || *result.AccountID == uuid.Nil {
		return uuid.Nil, personalErrors.ErrDeletionResultNotFound
	}
	return *result.AccountID, nil
}
func deletionPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{personalErrors.ErrDeletionPolicyNotConfigured, personalErrors.ErrInvalidDeletion, personalErrors.ErrDeletionState, personalErrors.ErrDeletionResultNotFound, personalErrors.ErrInvalidProof, personalErrors.ErrSessionRevoked, personalErrors.ErrAuthSessionScope, personalErrors.ErrAuthObjectUnavailable, shared.ErrPersonalScope, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	return shared.NewUnexpectedError(errors.New("no se pudo registrar o consultar la solicitud de eliminación"))
}
