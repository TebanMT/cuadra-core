//go:build server

package repositories

import (
	"context"
	"encoding/hex"
	"errors"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type LogoutPostgresRepository struct{ db *gorm.DB }

func NewLogoutRepository(db *gorm.DB) (*LogoutPostgresRepository, error) {
	if _, err := shared.NewPersonalPostgresUnitOfWork(db, shared.PersonalAuthRole); err != nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &LogoutPostgresRepository{db: db.Session(&gorm.Session{Logger: logger.Discard})}, nil
}

var _ repository.LogoutRepository = (*LogoutPostgresRepository)(nil)

func (r *LogoutPostgresRepository) Resolve(ctx context.Context, hash [32]byte) (id uuid.UUID, err error) {
	defer func() { err = logoutPersistenceError(err) }()
	if err = shared.ValidatePersonalAnonymousScope(ctx); err != nil {
		return uuid.Nil, err
	}
	if r == nil || r.db == nil || hash == [32]byte{} {
		return uuid.Nil, personalErrors.ErrInvalidLogout
	}
	var result struct{ AccountID *uuid.UUID }
	if err = r.db.WithContext(ctx).Raw("SELECT mancuerna_private.resolve_identity('logout',?) AS account_id", hex.EncodeToString(hash[:])).Scan(&result).Error; err != nil {
		return uuid.Nil, err
	}
	if result.AccountID == nil || *result.AccountID == uuid.Nil {
		return uuid.Nil, personalErrors.ErrInvalidLogout
	}
	return *result.AccountID, nil
}

func (*LogoutPostgresRepository) GetSessionForTicket(handle shared.Transaction, accountID uuid.UUID, hash [32]byte) (s *session.Session, err error) {
	defer func() { err = logoutPersistenceError(err) }()
	if hash == [32]byte{} {
		return nil, personalErrors.ErrInvalidLogout
	}
	tx, err := authSessionConnection(handle, accountID, true)
	if err != nil {
		return nil, err
	}
	// UNION deduplicates a ticket retained in both places for the same session.
	// An ambiguous association never selects an arbitrary session to revoke.
	var matches []struct{ SessionID uuid.UUID }
	err = tx.Raw(`SELECT id AS session_id FROM mancuerna.sessions WHERE account_id=? AND revocation_ticket_hash=?
 UNION SELECT session_id FROM mancuerna.session_rotations WHERE account_id=? AND old_revocation_ticket_hash=? LIMIT 2`, accountID, hash[:], accountID, hash[:]).Scan(&matches).Error
	if err != nil {
		return nil, err
	}
	if len(matches) != 1 {
		return nil, personalErrors.ErrInvalidLogout
	}
	var row models.PersonalSessionModel
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id=? AND id=?", accountID, matches[0].SessionID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, personalErrors.ErrInvalidLogout
	}
	if err != nil {
		return nil, err
	}
	return sessionToDomain(row)
}

func logoutPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{personalErrors.ErrInvalidLogout, personalErrors.ErrAuthSessionScope, shared.ErrPersonalScope, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	return shared.NewUnexpectedError(errors.New("no se pudo resolver el permiso de cierre de sesión"))
}
