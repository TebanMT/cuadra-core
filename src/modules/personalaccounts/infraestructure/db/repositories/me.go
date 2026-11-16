//go:build server

package repositories

import (
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/me"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type MePostgresRepository struct{}

func NewMeRepository() *MePostgresRepository { return &MePostgresRepository{} }

var _ repository.MeRepository = (*MePostgresRepository)(nil)

func (*MePostgresRepository) Get(handle shared.Transaction, accountID, sessionID, deviceID uuid.UUID) (result *me.Snapshot, err error) {
	defer func() { err = authSessionPersistenceError(err) }()
	if sessionID == uuid.Nil || deviceID == uuid.Nil {
		return nil, personalErrors.ErrAuthObjectUnavailable
	}
	tx, err := authSessionConnection(handle, accountID, false)
	if err != nil {
		return nil, err
	}
	// No raw E.164 value crosses the database boundary. Session, profile and
	// preparation state are read in one READ COMMITTED statement snapshot.
	var row struct {
		ID                   uuid.UUID
		DisplayName          string
		WeightUnit           string
		Revision             int64
		Status               string
		CreatedAt, UpdatedAt time.Time
		PhoneMasked          string
		SecurityVersion      int64
		RecoveryPrepared     bool
	}
	query := tx.Raw(`SELECT a.id,a.display_name,a.weight_unit,a.revision,a.status,a.created_at,a.updated_at,
 '•••• ' || right(a.phone_e164,4) AS phone_masked,a.security_version,
 EXISTS(SELECT 1 FROM mancuerna.recovery_keys k WHERE k.account_id=a.id
 AND k.consumed_at IS NULL AND k.revoked_at IS NULL AND k.prepared_at IS NOT NULL) AS recovery_prepared
 FROM mancuerna.accounts a WHERE a.id=? AND a.deleted_at IS NULL AND mancuerna_private.session_active(?,?)`, accountID, sessionID, deviceID).Scan(&row)
	if query.Error != nil {
		return nil, query.Error
	}
	if query.RowsAffected != 1 {
		return nil, personalErrors.ErrAuthObjectUnavailable
	}
	result = &me.Snapshot{Profile: account.Account{ID: row.ID, DisplayName: row.DisplayName, WeightUnit: account.WeightUnit(row.WeightUnit), Revision: row.Revision, Status: account.Status(row.Status), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, PhoneMasked: row.PhoneMasked, SecurityVersion: row.SecurityVersion, RecoveryPrepared: row.RecoveryPrepared}
	if result.Validate() != nil {
		return nil, personalErrors.ErrAuthObjectUnavailable
	}
	return result, nil
}
