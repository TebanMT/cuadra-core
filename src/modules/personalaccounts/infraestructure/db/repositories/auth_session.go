//go:build server

package repositories

import (
	"context"
	"database/sql"
	"errors"
	"time"

	deviceDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	port "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	sessionDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type AuthSessionPostgresRepository struct{}

func NewAuthSessionRepository() *AuthSessionPostgresRepository {
	return &AuthSessionPostgresRepository{}
}

var _ port.AuthSessionRepository = (*AuthSessionPostgresRepository)(nil)

func (*AuthSessionPostgresRepository) CreateDevice(handle shared.Transaction, accountID uuid.UUID, device *deviceDomain.AuthorizedDevice) (err error) {
	defer func() { err = authSessionPersistenceError(err) }()
	if err := device.Validate(); err != nil {
		return err
	}
	if device.AccountID != accountID || device.RevokedAt != nil || device.DeletedAt != nil || device.LastSeenAt != nil || !device.AuthorizedAt.Equal(device.CreatedAt) || !device.UpdatedAt.Equal(device.CreatedAt) {
		return personalErrors.ErrInvalidDevice
	}
	tx, err := authSessionConnection(handle, accountID, true)
	if err != nil {
		return err
	}
	var active bool
	err = tx.Raw(`SELECT EXISTS(SELECT 1 FROM mancuerna.accounts WHERE id=? AND status='active' AND deleted_at IS NULL) AND ?<=clock_timestamp()`, accountID, device.AuthorizedAt).Scan(&active).Error
	if err != nil {
		return err
	}
	if !active {
		return personalErrors.ErrAuthObjectUnavailable
	}
	row := deviceToModel(device)
	return authCreationError(tx.Create(&row).Error)
}

func (*AuthSessionPostgresRepository) CreateSession(handle shared.Transaction, accountID uuid.UUID, session *sessionDomain.Session) (err error) {
	defer func() { err = authSessionPersistenceError(err) }()
	if err := session.Validate(); err != nil {
		return err
	}
	if session.AccountID != accountID || session.Generation != 1 || session.RevokedAt != nil || session.LastUsedAt != nil || session.ExpiresAt.Sub(session.CreatedAt) != sessionDomain.InactivityTTL {
		return personalErrors.ErrInvalidSession
	}
	tx, err := authSessionConnection(handle, accountID, true)
	if err != nil {
		return err
	}
	var active bool
	err = tx.Raw(`SELECT EXISTS(SELECT 1 FROM mancuerna.accounts a JOIN mancuerna.devices d ON d.account_id=a.id
 WHERE a.id=? AND a.status='active' AND a.deleted_at IS NULL AND d.id=? AND d.revoked_at IS NULL AND d.deleted_at IS NULL AND d.authorized_at<=clock_timestamp())
 AND ?<=clock_timestamp() AND ?>clock_timestamp()`, accountID, session.DeviceID, session.CreatedAt, session.ExpiresAt).Scan(&active).Error
	if err != nil {
		return err
	}
	if !active {
		return personalErrors.ErrAuthObjectUnavailable
	}
	row := sessionToModel(session)
	return authCreationError(tx.Create(&row).Error)
}

func (*AuthSessionPostgresRepository) GetDevice(handle shared.Transaction, accountID, deviceID uuid.UUID) (device *deviceDomain.AuthorizedDevice, err error) {
	defer func() { err = authSessionPersistenceError(err) }()
	tx, err := authSessionConnection(handle, accountID, false)
	if err != nil {
		return nil, err
	}
	var row models.AuthorizedDeviceModel
	err = tx.Where("account_id=? AND id=? AND deleted_at IS NULL", accountID, deviceID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, personalErrors.ErrAuthObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	return deviceToDomain(row)
}

func (*AuthSessionPostgresRepository) GetSession(handle shared.Transaction, accountID, sessionID uuid.UUID) (session *sessionDomain.Session, err error) {
	defer func() { err = authSessionPersistenceError(err) }()
	tx, err := authSessionConnection(handle, accountID, false)
	if err != nil {
		return nil, err
	}
	var row models.PersonalSessionModel
	err = tx.Where("account_id=? AND id=?", accountID, sessionID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, personalErrors.ErrAuthObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	return sessionToDomain(row)
}

func (r *AuthSessionPostgresRepository) RevokeDevice(handle shared.Transaction, accountID, deviceID uuid.UUID, now time.Time, reason string) (err error) {
	defer func() { err = authSessionPersistenceError(err) }()
	tx, err := authSessionConnection(handle, accountID, true)
	if err != nil {
		return err
	}
	device, err := r.GetDevice(handle, accountID, deviceID)
	if err != nil {
		return err
	}
	alreadyRevoked := device.RevokedAt != nil
	if err = device.Revoke(now, reason); err != nil {
		return err
	}
	if !alreadyRevoked {
		result := tx.Model(&models.AuthorizedDeviceModel{}).Where("account_id=? AND id=? AND revoked_at IS NULL AND deleted_at IS NULL", accountID, deviceID).
			Updates(map[string]any{"revoked_at": device.RevokedAt, "revocation_reason": device.RevocationReason, "updated_at": device.UpdatedAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return personalErrors.ErrAuthObjectUnavailable
		}
	}
	// Device revocation and all of its session consequences share this exact
	// account transaction. Existing revocation instants are never overwritten.
	return tx.Model(&models.PersonalSessionModel{}).Where("account_id=? AND device_id=? AND revoked_at IS NULL", accountID, deviceID).Update("revoked_at", now.UTC()).Error
}

func (r *AuthSessionPostgresRepository) RevokeSession(handle shared.Transaction, accountID, sessionID uuid.UUID, now time.Time) (err error) {
	defer func() { err = authSessionPersistenceError(err) }()
	tx, err := authSessionConnection(handle, accountID, true)
	if err != nil {
		return err
	}
	session, err := r.GetSession(handle, accountID, sessionID)
	if err != nil {
		return err
	}
	if session.RevokedAt != nil {
		return session.Revoke(now)
	}
	if err = session.Revoke(now); err != nil {
		return err
	}
	result := tx.Model(&models.PersonalSessionModel{}).Where("account_id=? AND id=? AND revoked_at IS NULL", accountID, sessionID).Update("revoked_at", session.RevokedAt)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return personalErrors.ErrAuthObjectUnavailable
	}
	return nil
}

func (*AuthSessionPostgresRepository) SessionActive(handle shared.Transaction, accountID, sessionID, deviceID uuid.UUID) (active bool, err error) {
	defer func() {
		if err != nil {
			active = false
		}
		err = authSessionPersistenceError(err)
	}()
	tx, err := authSessionConnection(handle, accountID, false)
	if err != nil {
		return false, err
	}
	if sessionID == uuid.Nil || deviceID == uuid.Nil {
		return false, nil
	}
	err = tx.Raw("SELECT mancuerna_private.session_active(?,?)", sessionID, deviceID).Scan(&active).Error
	return active, err
}

func (*AuthSessionPostgresRepository) SessionExpired(handle shared.Transaction, accountID, sessionID uuid.UUID) (expired bool, err error) {
	defer func() {
		if err != nil {
			expired = false
		}
		err = authSessionPersistenceError(err)
	}()
	// This is a read, but its terminal conclusion must serialize with refresh,
	// decision and revocation. Reacquiring the account lock is safe in the tx.
	tx, err := authSessionConnection(handle, accountID, true)
	if err != nil {
		return false, err
	}
	if sessionID == uuid.Nil {
		return false, personalErrors.ErrAuthObjectNotFound
	}
	err = tx.Raw(`SELECT expires_at<=clock_timestamp() FROM mancuerna.sessions WHERE account_id=? AND id=?`, accountID, sessionID).Row().Scan(&expired)
	if errors.Is(err, sql.ErrNoRows) {
		return false, personalErrors.ErrAuthObjectNotFound
	}
	return expired, err
}

// All writes lock the account stream, including handles whose scope was just
// bound by ResolveVerifiedAccess. These locks are reentrant inside its tx.
func authSessionConnection(handle shared.Transaction, accountID uuid.UUID, write bool) (*gorm.DB, error) {
	transaction, ok := handle.(*shared.GormTransaction)
	if !ok || transaction == nil || transaction.Tx == nil || transaction.Tx.Statement == nil || accountID == uuid.Nil {
		return nil, personalErrors.ErrAuthSessionScope
	}
	if _, active := transaction.Tx.Statement.ConnPool.(gorm.TxCommitter); !active {
		return nil, personalErrors.ErrAuthSessionScope
	}
	tx := transaction.Tx.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent), SkipDefaultTransaction: true})
	var scope struct {
		Account, Gym, Isolation, ReadOnly string
		AuthOnly                          bool
	}
	err := tx.Raw(`SELECT coalesce(current_setting('mancuerna.account_id',true),'') AS account,
 coalesce(current_setting('tinta.gym_id',true),'') AS gym,current_setting('transaction_isolation') AS isolation,current_setting('transaction_read_only') AS read_only,
 (pg_has_role(current_user,'mancuerna_auth','USAGE') AND NOT pg_has_role(current_user,'mancuerna_data','MEMBER')
 AND NOT pg_has_role(current_user,'mancuerna_identity','MEMBER')) AS auth_only`).Scan(&scope).Error
	if err != nil {
		return nil, err
	}
	if scope.Account != accountID.String() || scope.Gym != "" || !scope.AuthOnly || scope.Isolation != "read committed" || (write && scope.ReadOnly != "off") {
		return nil, personalErrors.ErrAuthSessionScope
	}
	if write {
		var locked uuid.UUID
		err = tx.Raw("SELECT account_id FROM mancuerna.sync_streams WHERE account_id=? FOR UPDATE", accountID).Row().Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, personalErrors.ErrAuthObjectUnavailable
		}
		if err != nil {
			return nil, err
		}
	}
	return tx, nil
}

func authCreationError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return personalErrors.ErrAuthObjectConflict
	}
	return err
}

func authSessionPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{personalErrors.ErrInvalidDevice, personalErrors.ErrInvalidSession, personalErrors.ErrAuthObjectNotFound, personalErrors.ErrAuthObjectUnavailable, personalErrors.ErrAuthObjectConflict, personalErrors.ErrAuthSessionScope, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	return shared.NewUnexpectedError(errors.New("no se pudo persistir o consultar la sesión personal"))
}

func deviceToModel(d *deviceDomain.AuthorizedDevice) models.AuthorizedDeviceModel {
	var reason *string
	if d.RevocationReason != "" {
		copy := d.RevocationReason
		reason = &copy
	}
	return models.AuthorizedDeviceModel{ID: d.ID, AccountID: d.AccountID, Platform: d.Platform, Label: d.Label, PublicKey: append([]byte(nil), d.PublicKey[:]...), AuthorizedAt: d.AuthorizedAt.UTC(), AuthorizedByMethod: string(d.AuthorizedByMethod), RevokedAt: utcPointer(d.RevokedAt), RevocationReason: reason, LastSeenAt: utcPointer(d.LastSeenAt), CreatedAt: d.CreatedAt.UTC(), UpdatedAt: d.UpdatedAt.UTC(), DeletedAt: utcPointer(d.DeletedAt)}
}

func deviceToDomain(row models.AuthorizedDeviceModel) (*deviceDomain.AuthorizedDevice, error) {
	if len(row.PublicKey) != 32 {
		return nil, personalErrors.ErrInvalidDevice
	}
	d := &deviceDomain.AuthorizedDevice{ID: row.ID, AccountID: row.AccountID, Platform: row.Platform, Label: row.Label, AuthorizedAt: row.AuthorizedAt.UTC(), AuthorizedByMethod: deviceDomain.AuthorizationMethod(row.AuthorizedByMethod), RevokedAt: utcPointer(row.RevokedAt), LastSeenAt: utcPointer(row.LastSeenAt), CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(), DeletedAt: utcPointer(row.DeletedAt)}
	copy(d.PublicKey[:], row.PublicKey)
	if row.RevocationReason != nil {
		d.RevocationReason = *row.RevocationReason
	}
	return d, d.Validate()
}

func sessionToModel(s *sessionDomain.Session) models.PersonalSessionModel {
	return models.PersonalSessionModel{ID: s.ID, AccountID: s.AccountID, DeviceID: s.DeviceID, RefreshHash: append([]byte(nil), s.RefreshHash[:]...), RefreshFamilyID: s.RefreshFamilyID, Generation: s.Generation, ExpiresAt: s.ExpiresAt.UTC(), RevocationTicketHash: append([]byte(nil), s.RevocationTicketHash[:]...), RevokedAt: utcPointer(s.RevokedAt), LastUsedAt: utcPointer(s.LastUsedAt), CreatedAt: s.CreatedAt.UTC()}
}

func sessionToDomain(row models.PersonalSessionModel) (*sessionDomain.Session, error) {
	if len(row.RefreshHash) != 32 || len(row.RevocationTicketHash) != 32 {
		return nil, personalErrors.ErrInvalidSession
	}
	s := &sessionDomain.Session{ID: row.ID, AccountID: row.AccountID, DeviceID: row.DeviceID, RefreshFamilyID: row.RefreshFamilyID, Generation: row.Generation, ExpiresAt: row.ExpiresAt.UTC(), RevokedAt: utcPointer(row.RevokedAt), LastUsedAt: utcPointer(row.LastUsedAt), CreatedAt: row.CreatedAt.UTC()}
	copy(s.RefreshHash[:], row.RefreshHash)
	copy(s.RevocationTicketHash[:], row.RevocationTicketHash)
	return s, s.Validate()
}
