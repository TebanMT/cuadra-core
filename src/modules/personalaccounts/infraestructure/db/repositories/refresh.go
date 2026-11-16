//go:build server

package repositories

import (
	"context"
	"encoding/hex"
	"errors"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/refresh"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// The pool is validated once and private; only this fixed identity query can
// run before establishing a personal account scope. No token plaintext is used.
type RefreshIdentityPostgresResolver struct{ db *gorm.DB }

func NewRefreshIdentityResolver(db *gorm.DB) (*RefreshIdentityPostgresResolver, error) {
	if _, err := shared.NewPersonalPostgresUnitOfWork(db, shared.PersonalAuthRole); err != nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &RefreshIdentityPostgresResolver{db: db.Session(&gorm.Session{Logger: logger.Discard})}, nil
}
func (r *RefreshIdentityPostgresResolver) Resolve(ctx context.Context, hash [32]byte) (id uuid.UUID, err error) {
	defer func() { err = refreshPersistenceError(err) }()
	if r == nil || r.db == nil || hash == [32]byte{} {
		return uuid.Nil, personalErrors.ErrInvalidRefresh
	}
	if err = shared.ValidatePersonalAnonymousScope(ctx); err != nil {
		return uuid.Nil, err
	}
	var result struct{ AccountID *uuid.UUID }
	if err = r.db.WithContext(ctx).Raw("SELECT mancuerna_private.resolve_identity('refresh',?) AS account_id", hex.EncodeToString(hash[:])).Scan(&result).Error; err != nil {
		return uuid.Nil, err
	}
	if result.AccountID == nil || *result.AccountID == uuid.Nil {
		return uuid.Nil, personalErrors.ErrInvalidRefresh
	}
	return *result.AccountID, nil
}

var _ repository.RefreshIdentityResolver = (*RefreshIdentityPostgresResolver)(nil)

type RefreshSessionPostgresRepository struct{}

func NewRefreshSessionRepository() *RefreshSessionPostgresRepository {
	return &RefreshSessionPostgresRepository{}
}

var _ repository.RefreshSessionRepository = (*RefreshSessionPostgresRepository)(nil)

func (*RefreshSessionPostgresRepository) GetForUpdate(handle shared.Transaction, accountID uuid.UUID, hash [32]byte) (s *session.Session, r *refresh.Rotation, err error) {
	defer func() { err = refreshPersistenceError(err) }()
	if hash == [32]byte{} {
		return nil, nil, personalErrors.ErrInvalidRefresh
	}
	tx, err := authSessionConnection(handle, accountID, true)
	if err != nil {
		return nil, nil, err
	}
	var current models.PersonalSessionModel
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id=? AND refresh_hash=?", accountID, hash[:]).Take(&current).Error
	if err == nil {
		s, err = sessionToDomain(current)
		return s, nil, err
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, err
	}
	var history models.SessionRotationModel
	err = tx.Where("account_id=? AND old_refresh_hash=?", accountID, hash[:]).Take(&history).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, personalErrors.ErrInvalidRefresh
	}
	if err != nil {
		return nil, nil, err
	}
	r, err = rotationToDomain(history)
	if err != nil {
		return nil, nil, err
	}
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id=? AND id=?", accountID, r.SessionID).Take(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, personalErrors.ErrInvalidRefresh
	}
	if err != nil {
		return nil, nil, err
	}
	s, err = sessionToDomain(current)
	if err != nil {
		return nil, nil, err
	}
	if s.Generation <= r.Generation {
		return nil, nil, personalErrors.ErrInvalidRefresh
	}
	return s, r, nil
}

func (*RefreshSessionPostgresRepository) SaveRotation(handle shared.Transaction, accountID uuid.UUID, s *session.Session, r *refresh.Rotation) (err error) {
	defer func() { err = refreshPersistenceError(err) }()
	if s.Validate() != nil || r.Validate() != nil || s.AccountID != accountID || r.AccountID != accountID || s.ID != r.SessionID || s.Generation != r.Generation+1 || r.OldRevocationTicketHash == nil || len(r.Response.Ciphertext) == 0 || s.RefreshHash == r.OldRefreshHash || s.RevocationTicketHash == *r.OldRevocationTicketHash || s.RevokedAt != nil || s.LastUsedAt == nil || !s.LastUsedAt.Equal(r.CreatedAt) || !s.ExpiresAt.Equal(r.CreatedAt.Add(session.InactivityTTL)) {
		return personalErrors.ErrInvalidRefresh
	}
	tx, err := authSessionConnection(handle, accountID, true)
	if err != nil {
		return err
	}
	var previous models.PersonalSessionModel
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id=? AND id=?", accountID, s.ID).Take(&previous).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return personalErrors.ErrInvalidRefresh
	}
	if err != nil {
		return err
	}
	before, err := sessionToDomain(previous)
	if err != nil {
		return err
	}
	if before.DeviceID != s.DeviceID || before.RefreshFamilyID != s.RefreshFamilyID || !before.CreatedAt.Equal(s.CreatedAt) || before.Generation != r.Generation || before.RefreshHash != r.OldRefreshHash || before.RevocationTicketHash != *r.OldRevocationTicketHash || !before.ActiveAt(r.CreatedAt) {
		return personalErrors.ErrInvalidRefresh
	}
	var active bool
	if err = tx.Raw("SELECT mancuerna_private.session_active(?,?) AND ?<=clock_timestamp()", s.ID, s.DeviceID, r.CreatedAt).Scan(&active).Error; err != nil {
		return err
	}
	if !active {
		return personalErrors.ErrSessionRevoked
	}
	// Record history before CAS. Any error must propagate to the outer UoW so
	// history, token replacement and its audit event are committed together.
	row := rotationToModel(r)
	if err = tx.Create(&row).Error; err != nil {
		return err
	}
	updated := tx.Model(&models.PersonalSessionModel{}).Where("account_id=? AND id=? AND generation=? AND refresh_hash=? AND revoked_at IS NULL", accountID, s.ID, r.Generation, r.OldRefreshHash[:]).Updates(map[string]any{"refresh_hash": s.RefreshHash[:], "revocation_ticket_hash": s.RevocationTicketHash[:], "generation": s.Generation, "expires_at": s.ExpiresAt, "last_used_at": s.LastUsedAt})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return personalErrors.ErrInvalidRefresh
	}
	return nil
}

func refreshPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{personalErrors.ErrInvalidRefresh, personalErrors.ErrSessionRevoked, personalErrors.ErrAuthSessionScope, shared.ErrPersonalScope, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return personalErrors.ErrInvalidProof
	}
	return shared.NewUnexpectedError(errors.New("no se pudo consultar o guardar la renovación personal"))
}
func rotationToModel(r *refresh.Rotation) models.SessionRotationModel {
	m := models.SessionRotationModel{AccountID: r.AccountID, SessionID: r.SessionID, Generation: r.Generation, OldRefreshHash: append([]byte(nil), r.OldRefreshHash[:]...), OperationID: r.OperationID, RequestHash: append([]byte(nil), r.RequestHash[:]...), ResponseCiphertext: append([]byte(nil), r.Response.Ciphertext...), ResponseExpiresAt: r.ResponseExpiresAt, CreatedAt: r.CreatedAt}
	if r.OldRevocationTicketHash != nil {
		m.OldRevocationTicketHash = append([]byte(nil), r.OldRevocationTicketHash[:]...)
	}
	if r.Response.KeyVersion != "" {
		v := r.Response.KeyVersion
		m.KeyVersion = &v
	}
	return m
}
func rotationToDomain(m models.SessionRotationModel) (*refresh.Rotation, error) {
	if len(m.OldRefreshHash) != 32 || len(m.RequestHash) != 32 || (len(m.OldRevocationTicketHash) != 0 && len(m.OldRevocationTicketHash) != 32) {
		return nil, personalErrors.ErrInvalidRefresh
	}
	r := &refresh.Rotation{AccountID: m.AccountID, SessionID: m.SessionID, Generation: m.Generation, OldRefreshHash: [32]byte(m.OldRefreshHash), OperationID: m.OperationID, RequestHash: [32]byte(m.RequestHash), Response: security.Envelope{Ciphertext: append([]byte(nil), m.ResponseCiphertext...)}, ResponseExpiresAt: m.ResponseExpiresAt.UTC(), CreatedAt: m.CreatedAt.UTC()}
	if len(m.OldRevocationTicketHash) == 32 {
		v := [32]byte(m.OldRevocationTicketHash)
		r.OldRevocationTicketHash = &v
	}
	if m.KeyVersion != nil {
		r.Response.KeyVersion = *m.KeyVersion
	}
	return r, r.Validate()
}
