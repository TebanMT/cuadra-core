//go:build server

package repositories

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
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

type DeviceAuthorizationPostgresRepository struct{}

func NewDeviceAuthorizationRepository() *DeviceAuthorizationPostgresRepository {
	return &DeviceAuthorizationPostgresRepository{}
}

var _ repository.DeviceAuthorizationRepository = (*DeviceAuthorizationPostgresRepository)(nil)

func authorizationConnection(handle shared.Transaction, accountID uuid.UUID) (*gorm.DB, error) {
	tx, err := authSessionConnection(handle, accountID, true)
	if err != nil {
		return nil, err
	}
	var active bool
	err = tx.Raw("SELECT EXISTS(SELECT 1 FROM mancuerna.accounts WHERE id=? AND status='active' AND deleted_at IS NULL)", accountID).Scan(&active).Error
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, personalErrors.ErrAuthObjectUnavailable
	}
	return tx, nil
}
func authorizationAttemptConnection(handle shared.Transaction, accountID uuid.UUID, binding attempt.Binding) (*gorm.DB, error) {
	if binding.Validate() != nil {
		return nil, personalErrors.ErrAuthAttemptNotFound
	}
	tx, err := authorizationConnection(handle, accountID)
	if err != nil {
		return nil, err
	}
	var matches bool
	err = tx.Raw(`SELECT coalesce(current_setting('mancuerna.attempt_id',true),'')=? AND coalesce(current_setting('mancuerna.attempt_hash',true),'')=?`, binding.ID.String(), hex.EncodeToString(binding.ContinuationHash[:])).Scan(&matches).Error
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, personalErrors.ErrAuthSessionScope
	}
	return tx, nil
}
func authorizationActorConnection(handle shared.Transaction, actor security.AccessClaims) (*gorm.DB, error) {
	if !deviceauthorization.ValidID(actor.SessionID) || !deviceauthorization.ValidID(actor.DeviceID) {
		return nil, personalErrors.ErrInvalidProof
	}
	tx, err := authorizationConnection(handle, actor.AccountID)
	if err != nil {
		return nil, err
	}
	var clean, active bool
	err = tx.Raw(`SELECT coalesce(current_setting('mancuerna.attempt_id',true),'')='' AND coalesce(current_setting('mancuerna.attempt_hash',true),'')=''`).Scan(&clean).Error
	if err != nil {
		return nil, err
	}
	if !clean {
		return nil, personalErrors.ErrAuthSessionScope
	}
	err = tx.Raw("SELECT mancuerna_private.session_active(?,?)", actor.SessionID, actor.DeviceID).Scan(&active).Error
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, personalErrors.ErrSessionRevoked
	}
	return tx, nil
}

const authorizationProjection = "a.id,a.account_id,a.challenge_id,a.approved_by_device_id,a.comparison_code,a.status,a.expires_at,a.decided_at,a.created_at,c.candidate_device_id,c.candidate_public_key,c.candidate_platform,c.candidate_label"

func authorizationQuery(tx *gorm.DB, accountID uuid.UUID) *gorm.DB {
	return tx.Table("mancuerna.device_authorizations AS a").Joins("JOIN mancuerna.auth_challenges c ON c.account_id=a.account_id AND c.id=a.challenge_id").Where("a.account_id=?", accountID)
}

func (*DeviceAuthorizationPostgresRepository) Create(handle shared.Transaction, binding attempt.Binding, a *deviceauthorization.Authorization) (err error) {
	defer func() { err = authorizationPersistenceError(err) }()
	if a.Validate() != nil || a.Status != deviceauthorization.Pending || a.AttemptID != binding.ID {
		return personalErrors.ErrInvalidDeviceAuthorization
	}
	tx, err := authorizationAttemptConnection(handle, a.AccountID, binding)
	if err != nil {
		return err
	}
	var matches bool
	err = tx.Raw(`SELECT EXISTS(SELECT 1 FROM mancuerna.auth_challenges c JOIN mancuerna.accounts owner ON owner.id=c.account_id
 WHERE c.id=? AND c.account_id=? AND c.continuation_hash=? AND c.status='verified' AND c.purpose='access' AND c.verified_at IS NOT NULL
 AND c.expires_at>clock_timestamp() AND owner.phone_e164=c.destination_e164
 AND c.candidate_device_id=? AND c.candidate_public_key=? AND c.candidate_platform=? AND c.candidate_label=?
 AND ?<=c.expires_at AND ?>clock_timestamp() AND ?>=c.verified_at AND ?<=clock_timestamp())`, binding.ID, a.AccountID, binding.ContinuationHash[:], a.Candidate.ID, a.Candidate.PublicKey[:], a.Candidate.Platform, a.Candidate.Label, a.ExpiresAt, a.ExpiresAt, a.CreatedAt, a.CreatedAt).Scan(&matches).Error
	if err != nil {
		return err
	}
	if !matches {
		return personalErrors.ErrDeviceAuthorizationState
	}
	row := authorizationToModel(a)
	return tx.Create(&row).Error
}

func (*DeviceAuthorizationPostgresRepository) GetForAttempt(handle shared.Transaction, accountID uuid.UUID, binding attempt.Binding) (a *deviceauthorization.Authorization, err error) {
	defer func() {
		err = authorizationPersistenceError(err)
		if err != nil {
			a = nil
		}
	}()
	tx, err := authorizationAttemptConnection(handle, accountID, binding)
	if err != nil {
		return nil, err
	}
	var row models.DeviceAuthorizationProjection
	err = authorizationQuery(tx, accountID).Select(authorizationProjection).Where("c.id=? AND c.continuation_hash=?", binding.ID, binding.ContinuationHash[:]).Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "a"}}).Take(&row).Error
	if err != nil {
		return nil, err
	}
	return authorizationToDomain(row)
}

func (*DeviceAuthorizationPostgresRepository) GetForActor(handle shared.Transaction, actor security.AccessClaims, id uuid.UUID) (a *deviceauthorization.Authorization, binding attempt.Binding, err error) {
	defer func() {
		err = authorizationPersistenceError(err)
		if err != nil {
			a = nil
			binding = attempt.Binding{}
		}
	}()
	tx, err := authorizationActorConnection(handle, actor)
	if err != nil {
		return nil, binding, err
	}
	var row models.DeviceAuthorizationProjection
	err = authorizationQuery(tx, actor.AccountID).Select(authorizationProjection+",c.continuation_hash").Where("a.id=?", id).Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "a"}}).Take(&row).Error
	if err != nil {
		return nil, binding, err
	}
	if len(row.ContinuationHash) != 32 {
		return nil, binding, personalErrors.ErrInvalidDeviceAuthorization
	}
	a, err = authorizationToDomain(row)
	if err != nil {
		return nil, binding, err
	}
	binding = attempt.Binding{ID: a.AttemptID, ContinuationHash: [32]byte(row.ContinuationHash)}
	if binding.Validate() != nil {
		return nil, attempt.Binding{}, personalErrors.ErrInvalidDeviceAuthorization
	}
	return a, binding, nil
}

func (r *DeviceAuthorizationPostgresRepository) SaveDecision(handle shared.Transaction, actor security.AccessClaims, a *deviceauthorization.Authorization) (err error) {
	defer func() { err = authorizationPersistenceError(err) }()
	if a.Validate() != nil || a.AccountID != actor.AccountID || (a.Status != deviceauthorization.Approved && a.Status != deviceauthorization.Denied) || a.Candidate.ID == actor.DeviceID {
		return personalErrors.ErrInvalidDeviceAuthorization
	}
	if a.Status == deviceauthorization.Approved && *a.ApprovedByDeviceID != actor.DeviceID {
		return personalErrors.ErrInvalidDeviceAuthorization
	}
	tx, err := authorizationActorConnection(handle, actor)
	if err != nil {
		return err
	}
	current, _, err := r.GetForActor(handle, actor, a.ID)
	if err != nil {
		return err
	}
	if current.Status != deviceauthorization.Pending || current.AttemptID != a.AttemptID || current.Candidate != a.Candidate || current.ComparisonCode != a.ComparisonCode || !current.CreatedAt.Equal(a.CreatedAt) || !current.ExpiresAt.Equal(a.ExpiresAt) {
		return personalErrors.ErrDeviceAuthorizationState
	}
	// Recheck the phone under the account mutex; an old-number OTP cannot
	// authorize a candidate after a completed phone change. The caller saved
	// the challenge's consumed/cancelled state earlier in this same transaction.
	result := tx.Model(&models.DeviceAuthorizationModel{}).Where(`account_id=? AND id=? AND status='pending' AND expires_at>clock_timestamp() AND ?<=clock_timestamp()
 AND EXISTS(SELECT 1 FROM mancuerna.auth_challenges c JOIN mancuerna.accounts owner ON owner.id=c.account_id WHERE c.id=? AND c.account_id=? AND c.purpose='access' AND c.verified_at IS NOT NULL AND c.expires_at>clock_timestamp() AND c.destination_e164=owner.phone_e164 AND c.status=?)`, actor.AccountID, a.ID, a.DecidedAt, a.AttemptID, actor.AccountID, map[deviceauthorization.Status]string{deviceauthorization.Approved: "consumed", deviceauthorization.Denied: "cancelled"}[a.Status]).Updates(map[string]any{"status": string(a.Status), "approved_by_device_id": a.ApprovedByDeviceID, "decided_at": a.DecidedAt})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return personalErrors.ErrDeviceAuthorizationState
	}
	return nil
}

func (*DeviceAuthorizationPostgresRepository) StreamEpoch(handle shared.Transaction, actor security.AccessClaims) (epoch uuid.UUID, err error) {
	defer func() {
		err = authorizationPersistenceError(err)
		if err != nil {
			epoch = uuid.Nil
		}
	}()
	tx, err := authorizationActorConnection(handle, actor)
	if err != nil {
		return uuid.Nil, err
	}
	err = tx.Raw("SELECT epoch FROM mancuerna.sync_streams WHERE account_id=?", actor.AccountID).Row().Scan(&epoch)
	if err == nil && !deviceauthorization.ValidID(epoch) {
		return uuid.Nil, personalErrors.ErrInvalidDeviceAuthorization
	}
	return epoch, err
}
func (*DeviceAuthorizationPostgresRepository) List(handle shared.Transaction, actor security.AccessClaims, after *deviceauthorization.ListPosition, limit int) (items []deviceauthorization.Authorization, err error) {
	defer func() {
		err = authorizationPersistenceError(err)
		if err != nil {
			items = nil
		}
	}()
	if limit < 1 || limit > 101 || (after != nil && after.Validate() != nil) {
		return nil, personalErrors.ErrInvalidDeviceAuthorization
	}
	tx, err := authorizationActorConnection(handle, actor)
	if err != nil {
		return nil, err
	}
	query := authorizationQuery(tx, actor.AccountID).Select(authorizationProjection).Where(`a.status='pending' AND a.expires_at>clock_timestamp() AND c.status='verified' AND c.purpose='access' AND c.expires_at>clock_timestamp()
 AND EXISTS(SELECT 1 FROM mancuerna.accounts owner WHERE owner.id=c.account_id AND owner.phone_e164=c.destination_e164)`)
	if after != nil {
		query = query.Where("(a.created_at,a.id)<(?,?)", after.CreatedAt.UTC(), after.ID)
	}
	var rows []models.DeviceAuthorizationProjection
	if err = query.Order("a.created_at DESC,a.id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items = make([]deviceauthorization.Authorization, 0, len(rows))
	for _, row := range rows {
		a, err := authorizationToDomain(row)
		if err != nil {
			return nil, err
		}
		items = append(items, *a)
	}
	return items, nil
}

func authorizationPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{personalErrors.ErrInvalidDeviceAuthorization, personalErrors.ErrDeviceAuthorizationNotFound, personalErrors.ErrDeviceAuthorizationState, personalErrors.ErrDeviceAuthorizationExpired, personalErrors.ErrAuthAttemptNotFound, personalErrors.ErrInvalidProof, personalErrors.ErrSessionRevoked, personalErrors.ErrAuthSessionScope, personalErrors.ErrAuthObjectUnavailable, shared.ErrPersonalScope, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return personalErrors.ErrDeviceAuthorizationNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return personalErrors.ErrDeviceAuthorizationState
	}
	return shared.NewUnexpectedError(errors.New("no se pudo consultar o guardar la solicitud de autorización"))
}
func authorizationToModel(a *deviceauthorization.Authorization) models.DeviceAuthorizationModel {
	return models.DeviceAuthorizationModel{ID: a.ID, AccountID: a.AccountID, ChallengeID: a.AttemptID, ApprovedByDeviceID: attemptUUID(a.ApprovedByDeviceID), ComparisonCode: a.ComparisonCode, Status: string(a.Status), ExpiresAt: a.ExpiresAt.UTC(), DecidedAt: utcPointer(a.DecidedAt), CreatedAt: a.CreatedAt.UTC()}
}
func authorizationToDomain(row models.DeviceAuthorizationProjection) (*deviceauthorization.Authorization, error) {
	if len(row.CandidatePublicKey) != 32 {
		return nil, personalErrors.ErrInvalidDeviceAuthorization
	}
	a := &deviceauthorization.Authorization{ID: row.ID, AccountID: row.AccountID, AttemptID: row.ChallengeID, Candidate: attempt.Candidate{ID: row.CandidateDeviceID, PublicKey: [32]byte(row.CandidatePublicKey), Platform: row.CandidatePlatform, Label: row.CandidateLabel}, ComparisonCode: row.ComparisonCode, Status: deviceauthorization.Status(row.Status), ApprovedByDeviceID: attemptUUID(row.ApprovedByDeviceID), ExpiresAt: row.ExpiresAt.UTC(), DecidedAt: utcPointer(row.DecidedAt), CreatedAt: row.CreatedAt.UTC()}
	return a, a.Validate()
}
