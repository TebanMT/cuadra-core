//go:build server

package repositories

import (
	"errors"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	personalRepository "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type AuthAttemptPostgresRepository struct{}

var _ personalRepository.AuthAttemptRepository = (*AuthAttemptPostgresRepository)(nil)

func NewAuthAttemptPostgresRepository() *AuthAttemptPostgresRepository {
	return &AuthAttemptPostgresRepository{}
}

func attemptConnection(tx sharedDomain.Transaction) *gorm.DB {
	// GORM's SQL diagnostics can include full bound values even if the error
	// returned below is redacted. Keep this credential-bearing table silent.
	return tx.(*sharedDomain.GormTransaction).Tx.Session(&gorm.Session{Logger: logger.Discard})
}

func (r *AuthAttemptPostgresRepository) Create(tx sharedDomain.Transaction, a *attempt.AuthAttempt) error {
	if err := a.Validate(); err != nil {
		return sharedDomain.NewValidationError(err)
	}
	if a.Revision != 1 || a.State != attempt.Pending || a.ProviderOperation != nil || a.ProviderReference != "" || a.Attempts != 0 {
		return sharedDomain.NewValidationError(personalErrors.ErrInvalidAuthAttempt)
	}
	row := attemptToModel(a)
	// OnConflict does not overwrite another candidate or request sharing an ID.
	result := attemptConnection(tx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(&row)
	if result.Error != nil {
		return attemptPersistenceError(result.Error)
	}
	if result.RowsAffected != 1 {
		return sharedDomain.NewBusinessError(personalErrors.ErrAuthAttemptConflict, "")
	}
	return nil
}
func (r *AuthAttemptPostgresRepository) Get(tx sharedDomain.Transaction, binding attempt.Binding) (*attempt.AuthAttempt, error) {
	return r.get(tx, binding, false)
}
func (r *AuthAttemptPostgresRepository) GetForUpdate(tx sharedDomain.Transaction, binding attempt.Binding) (*attempt.AuthAttempt, error) {
	return r.get(tx, binding, true)
}
func (r *AuthAttemptPostgresRepository) get(tx sharedDomain.Transaction, binding attempt.Binding, lock bool) (*attempt.AuthAttempt, error) {
	if binding.Validate() != nil {
		return nil, sharedDomain.NewBusinessError(personalErrors.ErrAuthAttemptNotFound, "")
	}
	query := attemptConnection(tx).Where("id = ? AND continuation_hash = ?", binding.ID, binding.ContinuationHash[:])
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row models.AuthAttemptModel
	if err := query.Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, sharedDomain.NewBusinessError(personalErrors.ErrAuthAttemptNotFound, "")
		}
		return nil, attemptPersistenceError(err)
	}
	a, err := attemptToDomain(&row)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(personalErrors.ErrInvalidAuthAttempt)
	}
	return a, nil
}

func sameAttemptIdentity(a, b *attempt.AuthAttempt) bool {
	return a.Binding == b.Binding && a.ProposedAccountID == b.ProposedAccountID && a.Candidate == b.Candidate && a.Nonce == b.Nonce && a.Purpose == b.Purpose && a.DestinationE164 == b.DestinationE164 && a.RequestHash == b.RequestHash && a.CreatedAt.Equal(b.CreatedAt)
}

func (r *AuthAttemptPostgresRepository) Save(tx sharedDomain.Transaction, binding attempt.Binding, a *attempt.AuthAttempt, expectedRevision int64) error {
	if a == nil || a.Binding != binding {
		return sharedDomain.NewBusinessError(personalErrors.ErrAuthAttemptNotFound, "")
	}
	if err := a.Validate(); err != nil {
		return sharedDomain.NewValidationError(err)
	}
	if expectedRevision < 1 || a.Revision <= expectedRevision {
		return sharedDomain.NewValidationError(personalErrors.ErrInvalidAuthAttempt)
	}
	current, err := r.GetForUpdate(tx, binding)
	if err != nil {
		return err
	}
	if current.Revision != expectedRevision {
		return sharedDomain.NewBusinessError(personalErrors.ErrAuthAttemptConflict, "")
	}
	if current.State == attempt.Consumed || current.State == attempt.Cancelled || current.State == attempt.Expired {
		return sharedDomain.NewBusinessError(personalErrors.ErrAuthAttemptState, "")
	}
	if !sameAttemptIdentity(current, a) || a.ExpiresAt.After(current.ExpiresAt) || a.Attempts < current.Attempts {
		return sharedDomain.NewValidationError(personalErrors.ErrInvalidAuthAttempt)
	}
	if (current.ProviderReference != "" && a.ProviderReference == "") || (current.VerifiedAt != nil && (a.VerifiedAt == nil || !current.VerifiedAt.Equal(*a.VerifiedAt))) {
		return sharedDomain.NewValidationError(personalErrors.ErrInvalidAuthAttempt)
	}
	if old := current.ProviderOperation; old != nil {
		next := a.ProviderOperation
		if next == nil || next.StartedAt.Before(old.StartedAt) || (old.ID == next.ID && (old.RequestHash != next.RequestHash || old.Kind != next.Kind || !old.StartedAt.Equal(next.StartedAt) || current.Channel != a.Channel)) {
			return sharedDomain.NewValidationError(personalErrors.ErrInvalidAuthAttempt)
		}
		if old.ID == next.ID && old.Kind == attempt.Check && current.ProviderReference != a.ProviderReference {
			return sharedDomain.NewValidationError(personalErrors.ErrInvalidAuthAttempt)
		}
	}
	if current.AccountID != nil && (a.AccountID == nil || *current.AccountID != *a.AccountID) {
		return sharedDomain.NewValidationError(personalErrors.ErrInvalidAuthAttempt)
	}
	if current.AccountID == nil && a.AccountID != nil && (current.State != attempt.Verified || (a.State != attempt.Verified && a.State != attempt.Consumed)) {
		return sharedDomain.NewValidationError(personalErrors.ErrInvalidAuthAttempt)
	}
	row := attemptToModel(a)
	// Immutable binding is compared above under a row lock and omitted from the
	// write. CAS and the credential predicate remain explicit despite RLS.
	result := attemptConnection(tx).Model(&models.AuthAttemptModel{}).
		Where("id = ? AND continuation_hash = ? AND revision = ?", binding.ID, binding.ContinuationHash[:], expectedRevision).
		Updates(map[string]any{"account_id": row.AccountID, "channel": row.Channel, "provider_reference": row.ProviderReference, "status": row.Status, "attempts": row.Attempts, "revision": row.Revision, "expires_at": row.ExpiresAt, "verified_at": row.VerifiedAt, "consumed_at": row.ConsumedAt, "result_reference": row.ResultReference, "provider_operation_id": row.ProviderOperationID, "provider_operation_hash": row.ProviderOperationHash, "provider_operation_kind": row.ProviderOperationKind, "provider_operation_started_at": row.ProviderOperationStartedAt})
	if result.Error != nil {
		return attemptPersistenceError(result.Error)
	}
	if result.RowsAffected != 1 {
		return sharedDomain.NewBusinessError(personalErrors.ErrAuthAttemptConflict, "")
	}
	return nil
}

// Do not expose database error detail: it can contain destination, nonce,
// continuation hashes or the provider reference as failed-row values.
func attemptPersistenceError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return sharedDomain.NewBusinessError(personalErrors.ErrAuthAttemptConflict, "")
	}
	return sharedDomain.NewUnexpectedError(errors.New("no se pudo persistir el intento de autenticación"))
}
func attemptUUID(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func attemptToModel(a *attempt.AuthAttempt) models.AuthAttemptModel {
	row := models.AuthAttemptModel{ID: a.ID, AccountID: attemptUUID(a.AccountID), ProposedAccountID: a.ProposedAccountID, CandidateDeviceID: a.Candidate.ID, Nonce: append([]byte(nil), a.Nonce[:]...), CandidatePublicKey: append([]byte(nil), a.Candidate.PublicKey[:]...), CandidatePlatform: a.Candidate.Platform, CandidateLabel: a.Candidate.Label, Purpose: string(a.Purpose), DestinationE164: a.DestinationE164, Channel: string(a.Channel), Status: string(a.State), ContinuationHash: append([]byte(nil), a.ContinuationHash[:]...), RequestHash: append([]byte(nil), a.RequestHash[:]...), Attempts: a.Attempts, Revision: a.Revision, ExpiresAt: a.ExpiresAt.UTC(), VerifiedAt: utcPointer(a.VerifiedAt), ConsumedAt: utcPointer(a.ConsumedAt), ResultReference: attemptUUID(a.ResultReference), CreatedAt: a.CreatedAt.UTC()}
	if a.ProviderReference != "" {
		reference := a.ProviderReference
		row.ProviderReference = &reference
	}
	if op := a.ProviderOperation; op != nil {
		id, kind, stamp := op.ID, string(op.Kind), op.StartedAt.UTC()
		row.ProviderOperationID, row.ProviderOperationKind, row.ProviderOperationStartedAt = &id, &kind, &stamp
		row.ProviderOperationHash = append([]byte(nil), op.RequestHash[:]...)
	}
	return row
}
func attemptToDomain(row *models.AuthAttemptModel) (*attempt.AuthAttempt, error) {
	if row == nil || len(row.ContinuationHash) != 32 || len(row.RequestHash) != 32 || len(row.Nonce) != 32 || len(row.CandidatePublicKey) != 32 {
		return nil, personalErrors.ErrInvalidAuthAttempt
	}
	a := &attempt.AuthAttempt{Binding: attempt.Binding{ID: row.ID, ContinuationHash: [32]byte(row.ContinuationHash)}, AccountID: attemptUUID(row.AccountID), ProposedAccountID: row.ProposedAccountID, Candidate: attempt.Candidate{ID: row.CandidateDeviceID, PublicKey: [32]byte(row.CandidatePublicKey), Platform: row.CandidatePlatform, Label: row.CandidateLabel}, Nonce: [32]byte(row.Nonce), Purpose: attempt.Purpose(row.Purpose), DestinationE164: row.DestinationE164, Channel: verification.Channel(row.Channel), RequestHash: [32]byte(row.RequestHash), State: attempt.State(row.Status), Attempts: row.Attempts, Revision: row.Revision, ExpiresAt: row.ExpiresAt.UTC(), VerifiedAt: utcPointer(row.VerifiedAt), ConsumedAt: utcPointer(row.ConsumedAt), ResultReference: attemptUUID(row.ResultReference), CreatedAt: row.CreatedAt.UTC()}
	if row.ProviderReference != nil {
		a.ProviderReference = *row.ProviderReference
	}
	if row.ProviderOperationID != nil || row.ProviderOperationHash != nil || row.ProviderOperationKind != nil || row.ProviderOperationStartedAt != nil {
		if row.ProviderOperationID == nil || len(row.ProviderOperationHash) != 32 || row.ProviderOperationKind == nil || row.ProviderOperationStartedAt == nil {
			return nil, personalErrors.ErrInvalidAuthAttempt
		}
		a.ProviderOperation = &attempt.ProviderOperation{ID: *row.ProviderOperationID, RequestHash: [32]byte(row.ProviderOperationHash), Kind: attempt.OperationKind(*row.ProviderOperationKind), StartedAt: row.ProviderOperationStartedAt.UTC()}
	}
	return a, a.Validate()
}
