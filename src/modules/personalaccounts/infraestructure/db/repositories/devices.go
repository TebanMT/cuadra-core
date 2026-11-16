//go:build server

package repositories

import (
	"context"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type DevicesPostgresRepository struct{}

func NewDevicesRepository() *DevicesPostgresRepository { return &DevicesPostgresRepository{} }

var _ repository.DevicesRepository = (*DevicesPostgresRepository)(nil)

func devicesConnection(handle shared.Transaction, actor security.AccessClaims) (*gorm.DB, error) {
	if actor.SessionID == uuid.Nil || actor.DeviceID == uuid.Nil {
		return nil, personalErrors.ErrInvalidDevice
	}
	tx, err := authSessionConnection(handle, actor.AccountID, true)
	if err != nil {
		return nil, err
	}
	var clean bool
	if err = tx.Raw(`SELECT coalesce(current_setting('mancuerna.attempt_id',true),'')='' AND coalesce(current_setting('mancuerna.attempt_hash',true),'')=''`).Scan(&clean).Error; err != nil {
		return nil, err
	}
	if !clean {
		return nil, personalErrors.ErrAuthSessionScope
	}
	var active bool
	if err = tx.Raw("SELECT mancuerna_private.session_active(?,?)", actor.SessionID, actor.DeviceID).Scan(&active).Error; err != nil {
		return nil, err
	}
	if !active {
		return nil, personalErrors.ErrSessionRevoked
	}
	return tx, nil
}

func (*DevicesPostgresRepository) StreamEpoch(handle shared.Transaction, actor security.AccessClaims) (epoch uuid.UUID, err error) {
	defer func() {
		err = devicesPersistenceError(err)
		if err != nil {
			epoch = uuid.Nil
		}
	}()
	tx, err := devicesConnection(handle, actor)
	if err != nil {
		return uuid.Nil, err
	}
	err = tx.Raw("SELECT epoch FROM mancuerna.sync_streams WHERE account_id=?", actor.AccountID).Row().Scan(&epoch)
	if err == nil && epoch == uuid.Nil {
		err = personalErrors.ErrInvalidDevice
	}
	return epoch, err
}

func (*DevicesPostgresRepository) List(handle shared.Transaction, actor security.AccessClaims, after *device.ListPosition, limit int) (items []device.ListedDevice, err error) {
	defer func() {
		err = devicesPersistenceError(err)
		if err != nil {
			items = nil
		}
	}()
	if limit < 1 || limit > 101 || (after != nil && after.Validate() != nil) {
		return nil, personalErrors.ErrInvalidDevice
	}
	tx, err := devicesConnection(handle, actor)
	if err != nil {
		return nil, err
	}
	query := tx.Model(&models.ListedDeviceModel{}).Select("id,account_id,platform,label,authorized_at,revoked_at,last_seen_at").Where("account_id=? AND deleted_at IS NULL", actor.AccountID)
	if after != nil {
		query = query.Where("(authorized_at,id) < (?,?)", after.AuthorizedAt.UTC(), after.ID)
	}
	var rows []models.ListedDeviceModel
	if err = query.Order("authorized_at DESC,id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items = make([]device.ListedDevice, 0, len(rows))
	for _, row := range rows {
		item, err := listedDeviceToDomain(row)
		if err != nil || item.AccountID != actor.AccountID {
			return nil, personalErrors.ErrInvalidDevice
		}
		items = append(items, item)
	}
	return items, nil
}

func (*DevicesPostgresRepository) Revoke(handle shared.Transaction, actor security.AccessClaims, target device.RevocationTarget, now time.Time) (err error) {
	defer func() { err = devicesPersistenceError(err) }()
	if target.Validate() != nil || now.IsZero() || now.UTC().Year() < 1 || now.UTC().Year() > 9999 {
		return personalErrors.ErrInvalidDevice
	}
	tx, err := devicesConnection(handle, actor)
	if err != nil {
		return err
	}
	query := tx.Model(&models.ListedDeviceModel{}).Where("account_id=? AND deleted_at IS NULL", actor.AccountID)
	if target.DeviceID != uuid.Nil {
		query = query.Where("id=?", target.DeviceID)
	} else if target.Scope == device.RevokeOthers {
		query = query.Where("id<>?", actor.DeviceID)
	}
	var ids []uuid.UUID
	if err = query.Order("id").Pluck("id", &ids).Error; err != nil {
		return err
	}
	if target.DeviceID != uuid.Nil && len(ids) != 1 {
		return personalErrors.ErrAuthObjectNotFound
	}
	// Reuse the aggregate's validated transition and the existing implementation
	// which preserves the first revocation instant and withdraws every session.
	sessions := NewAuthSessionRepository()
	for _, id := range ids {
		if err = sessions.RevokeDevice(handle, actor.AccountID, id, now, "account_owner"); err != nil {
			return err
		}
	}
	// No actor check after this point: self/all revocation may have just retired
	// the actor. Authority was established under the same lock before the effect.
	// Never cancel a consumed authorization or revive a terminal challenge.
	// Provider reservation metadata remains untouched; revision invalidates any
	// delayed outcome that was reserved before this cancellation.
	return tx.Exec(`WITH cancelled AS (
 UPDATE mancuerna.device_authorizations a SET status='cancelled',decided_at=?
 FROM mancuerna.auth_challenges c
 WHERE a.account_id=? AND c.account_id=a.account_id AND c.id=a.challenge_id
 AND c.status IN ('pending','uncertain','verified')
 AND ((a.status='pending' AND NOT EXISTS(SELECT 1 FROM mancuerna.devices d WHERE d.account_id=a.account_id AND d.revoked_at IS NULL AND d.deleted_at IS NULL AND d.authorized_at<=clock_timestamp()))
 OR (a.status='approved' AND NOT EXISTS(SELECT 1 FROM mancuerna.devices d WHERE d.account_id=a.account_id AND d.id=a.approved_by_device_id AND d.revoked_at IS NULL AND d.deleted_at IS NULL AND d.authorized_at<=clock_timestamp())))
 RETURNING a.challenge_id
 ) UPDATE mancuerna.auth_challenges c SET status='cancelled',revision=revision+1
 WHERE c.account_id=? AND c.id IN (SELECT challenge_id FROM cancelled) AND c.status IN ('pending','uncertain','verified')`, now.UTC(), actor.AccountID, actor.AccountID).Error
}

func listedDeviceToDomain(m models.ListedDeviceModel) (device.ListedDevice, error) {
	d := device.ListedDevice{ID: m.ID, AccountID: m.AccountID, Platform: m.Platform, Label: m.Label, AuthorizedAt: m.AuthorizedAt.UTC(), RevokedAt: utcPointer(m.RevokedAt), LastSeenAt: utcPointer(m.LastSeenAt)}
	return d, d.Validate()
}

func devicesPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{personalErrors.ErrInvalidDevice, personalErrors.ErrSessionRevoked, personalErrors.ErrAuthObjectNotFound, personalErrors.ErrAuthObjectUnavailable, personalErrors.ErrAuthSessionScope, shared.ErrPersonalScope, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	return shared.NewUnexpectedError(errors.New("no se pudo consultar o revocar el dispositivo personal"))
}
