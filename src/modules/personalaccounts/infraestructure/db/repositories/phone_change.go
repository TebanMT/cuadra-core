//go:build server

package repositories

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/phonechange"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/refresh"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type PhoneChangePostgresRepository struct{ phones *IdentityAccessRepository }

func NewPhoneChangeRepository(key []byte) (*PhoneChangePostgresRepository, error) {
	p, err := NewIdentityAccessRepository(key)
	if err != nil {
		return nil, err
	}
	return &PhoneChangePostgresRepository{phones: p}, nil
}
func (*PhoneChangePostgresRepository) String() string {
	return "phone change repository (key redacted)"
}
func (r *PhoneChangePostgresRepository) GoString() string { return r.String() }

var _ repository.PhoneChangeRepository = (*PhoneChangePostgresRepository)(nil)

func phoneChangeConnection(h shared.Transaction, actor security.AccessClaims, write bool) (*gorm.DB, error) {
	if actor.SessionID == uuid.Nil || actor.DeviceID == uuid.Nil {
		return nil, personalErrors.ErrInvalidPhoneChange
	}
	tx, err := authSessionConnection(h, actor.AccountID, write)
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
	return tx, nil
}
func (*PhoneChangePostgresRepository) CurrentPhone(h shared.Transaction, actor security.AccessClaims) (phone string, err error) {
	defer func() { err = phoneChangeError(err) }()
	tx, err := phoneChangeConnection(h, actor, false)
	if err != nil {
		return "", err
	}
	err = tx.Raw(`SELECT phone_e164 FROM mancuerna.accounts WHERE id=? AND status='active' AND deleted_at IS NULL`, actor.AccountID).Row().Scan(&phone)
	if errors.Is(err, sql.ErrNoRows) {
		return "", personalErrors.ErrSessionRevoked
	}
	return phone, err
}
func (r *PhoneChangePostgresRepository) LockPhones(h shared.Transaction, actor security.AccessClaims, destination string) (o phonechange.Observation, err error) {
	defer func() { err = phoneChangeError(err) }()
	if r == nil || r.phones == nil || !accessPhoneFormat.MatchString(destination) {
		return o, personalErrors.ErrInvalidPhoneChange
	}
	tx, err := phoneChangeConnection(h, actor, false)
	if err != nil {
		return o, err
	}
	var rw bool
	if err = tx.Raw(`SELECT current_setting('transaction_read_only')='off'`).Scan(&rw).Error; err != nil {
		return o, err
	}
	if !rw {
		return o, personalErrors.ErrAuthSessionScope
	}
	o.Phone, err = r.CurrentPhone(h, actor)
	if err != nil {
		return o, err
	}
	if o.Phone == destination {
		return o, personalErrors.ErrInvalidPhoneChange
	}
	keys := []int64{r.phones.phoneLockKey(o.Phone), r.phones.phoneLockKey(destination)}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for i, key := range keys {
		if i > 0 && key == keys[i-1] {
			continue
		}
		if err = tx.Exec(`SELECT pg_advisory_xact_lock(?)`, key).Error; err != nil {
			return o, err
		}
	}
	return o, nil
}
func (r *PhoneChangePostgresRepository) ValidatePhones(h shared.Transaction, actor security.AccessClaims, destination string, o phonechange.Observation) (err error) {
	defer func() { err = phoneChangeError(err) }()
	if r == nil || r.phones == nil || !accessPhoneFormat.MatchString(destination) || !accessPhoneFormat.MatchString(o.Phone) || o.Phone == destination {
		return personalErrors.ErrInvalidPhoneChange
	}
	tx, err := phoneChangeConnection(h, actor, true)
	if err != nil {
		return err
	}
	current, err := r.CurrentPhone(h, actor)
	if err != nil {
		return err
	}
	if current != o.Phone {
		return personalErrors.ErrPhoneChangeRetry
	}
	for _, phone := range []string{o.Phone, destination} {
		key := uint64(r.phones.phoneLockKey(phone))
		var held bool
		err = tx.Raw(`SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND mode='ExclusiveLock' AND granted AND objsubid=1 AND classid::bigint=? AND objid::bigint=?)`, int64(key>>32), int64(key&0xffffffff)).Scan(&held).Error
		if err != nil {
			return err
		}
		if !held {
			return personalErrors.ErrAuthSessionScope
		}
	}
	var available bool
	if err = tx.Raw(`SELECT mancuerna_private.resolve_identity('phone',?) IS NULL`, destination).Scan(&available).Error; err != nil {
		return err
	}
	if !available {
		return personalErrors.ErrRecoveryPhoneUnavailable
	}
	return nil
}
func (*PhoneChangePostgresRepository) CheckInitiator(h shared.Transaction, actor security.AccessClaims, a *attempt.AuthAttempt) (err error) {
	defer func() { err = phoneChangeError(err) }()
	if phonechange.ValidateActor(a, actor) != nil {
		return personalErrors.ErrInvalidPhoneChange
	}
	tx, err := phoneChangeConnection(h, actor, false)
	if err != nil {
		return err
	}
	var rows []models.AuthReceiptModel
	if err = tx.Where(`account_id=? AND attempt_id=? AND purpose=? AND request_hash=? AND result_reference=?`, actor.AccountID, a.ID, phonechange.StartPurpose, a.RequestHash[:], actor.SessionID).Limit(2).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) != 1 {
		return personalErrors.ErrInvalidPhoneChange
	}
	r, err := receiptToDomain(rows[0])
	if err != nil {
		return err
	}
	return phonechange.MatchInitiator(a, r, actor)
}
func (*PhoneChangePostgresRepository) CheckProof(h shared.Transaction, actor security.AccessClaims, tokenHash, targetHash [32]byte, op uuid.UUID, now time.Time) (err error) {
	defer func() { err = phoneChangeError(err) }()
	tx, err := deviceProofConnection(h, actor)
	if err != nil {
		return err
	}
	var row models.DeviceProofModel
	err = tx.Where(`account_id=? AND session_id=? AND device_id=? AND token_hash=? AND expires_at>clock_timestamp()`, actor.AccountID, actor.SessionID, actor.DeviceID, tokenHash[:]).Take(&row).Error
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
	// Validate by consuming a detached domain value. No SQL mutation occurs.
	return p.Consume(actor, security.PhoneChange, targetHash, tokenHash, op, now)
}

func (r *PhoneChangePostgresRepository) Apply(h shared.Transaction, actor security.AccessClaims, binding attempt.Binding, o phonechange.Observation, next *session.Session, rotation *refresh.Rotation, proofHash [32]byte, now time.Time) (err error) {
	defer func() { err = phoneChangeError(err) }()
	if next.Validate() != nil || rotation.Validate() != nil || next.AccountID != actor.AccountID || next.ID != actor.SessionID || next.DeviceID != actor.DeviceID || next.Generation != rotation.Generation+1 || next.RevokedAt != nil || next.LastUsedAt == nil || !next.LastUsedAt.Equal(now) || !next.ExpiresAt.Equal(now.Add(session.InactivityTTL)) || rotation.AccountID != actor.AccountID || rotation.SessionID != actor.SessionID || rotation.OldRevocationTicketHash == nil || len(rotation.Response.Ciphertext) != 0 || !rotation.CreatedAt.Equal(now) {
		return personalErrors.ErrInvalidPhoneChange
	}
	tx, err := phoneChangeConnection(h, actor, true)
	if err != nil {
		return err
	}
	a, err := NewAuthAttemptPostgresRepository().GetForUpdate(h, binding)
	if err != nil {
		return err
	}
	if err = r.CheckInitiator(h, actor, a); err != nil {
		return err
	}
	if a.State != attempt.Verified || a.VerifiedAt == nil || a.ProviderReference == "" || a.ProviderOperation == nil || a.ProviderOperation.Kind != attempt.Check || a.ProviderOperation.ID != rotation.OperationID || a.ProviderOperation.RequestHash != rotation.RequestHash || !now.Before(a.ExpiresAt) {
		return personalErrors.ErrInvalidPhoneChange
	}
	if err = r.ValidatePhones(h, actor, a.DestinationE164, o); err != nil {
		return err
	}
	var allowed bool
	err = tx.Raw(`SELECT mancuerna_private.session_active(?,?) AND ?<=clock_timestamp()
 AND EXISTS(SELECT 1 FROM mancuerna.device_proofs p WHERE p.account_id=? AND p.session_id=? AND p.device_id=? AND p.purpose='phone_change' AND p.token_hash=? AND p.request_hash=? AND p.operation_id=? AND p.consumed_at=? AND p.expires_at>clock_timestamp())
 AND EXISTS(SELECT 1 FROM mancuerna.auth_receipts r WHERE r.account_id=? AND r.attempt_id=? AND r.operation_id=? AND r.purpose=? AND r.request_hash=? AND r.completed_at>=? AND r.completed_at<=clock_timestamp())`, actor.SessionID, actor.DeviceID, now, actor.AccountID, actor.SessionID, actor.DeviceID, proofHash[:], rotation.RequestHash[:], rotation.OperationID, now, actor.AccountID, a.ID, rotation.OperationID, phonechange.CompletePurpose, rotation.RequestHash[:], a.ProviderOperation.StartedAt).Scan(&allowed).Error
	if err != nil {
		return err
	}
	if !allowed {
		return personalErrors.ErrInvalidPhoneChange
	}
	var previous models.PersonalSessionModel
	if err = tx.Where(`account_id=? AND id=?`, actor.AccountID, actor.SessionID).Take(&previous).Error; err != nil {
		return err
	}
	before, err := sessionToDomain(previous)
	if err != nil {
		return err
	}
	if before.DeviceID != next.DeviceID || before.RefreshFamilyID != next.RefreshFamilyID || !before.CreatedAt.Equal(next.CreatedAt) || before.Generation != rotation.Generation || before.RefreshHash != rotation.OldRefreshHash || before.RevocationTicketHash != *rotation.OldRevocationTicketHash || !before.ActiveAt(now) || next.RefreshHash == before.RefreshHash || next.RevocationTicketHash == before.RevocationTicketHash {
		return personalErrors.ErrInvalidPhoneChange
	}
	row := rotationToModel(rotation)
	if err = tx.Create(&row).Error; err != nil {
		return err
	}
	updated := tx.Model(&models.PersonalSessionModel{}).Where(`account_id=? AND id=? AND generation=? AND refresh_hash=? AND revoked_at IS NULL`, actor.AccountID, actor.SessionID, before.Generation, before.RefreshHash[:]).Updates(map[string]any{"refresh_hash": next.RefreshHash[:], "revocation_ticket_hash": next.RevocationTicketHash[:], "generation": next.Generation, "last_used_at": now, "expires_at": next.ExpiresAt})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return personalErrors.ErrInvalidPhoneChange
	}
	updated = tx.Exec(`UPDATE mancuerna.accounts SET phone_e164=?,phone_verified_at=?,phone_changed_at=?,security_version=security_version+1,updated_at=? WHERE id=? AND phone_e164=? AND status='active' AND deleted_at IS NULL AND security_version<9223372036854775807`, a.DestinationE164, a.VerifiedAt, now, now, actor.AccountID, o.Phone)
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return personalErrors.ErrPhoneChangeRetry
	}
	if err = tx.Exec(`UPDATE mancuerna.sessions SET revoked_at=? WHERE account_id=? AND id<>? AND revoked_at IS NULL`, now, actor.AccountID, actor.SessionID).Error; err != nil {
		return err
	}
	// Only bound attempts visible under this account are invalidated. Consumed
	// attempts retain their immutable historical results and receipts.
	return tx.Exec(`WITH cancelled AS (
 UPDATE mancuerna.auth_challenges SET status='cancelled',revision=revision+1
 WHERE account_id=? AND id<>? AND status IN ('pending','uncertain','verified') RETURNING id)
 UPDATE mancuerna.device_authorizations SET status='cancelled',decided_at=?
 WHERE account_id=? AND challenge_id IN (SELECT id FROM cancelled) AND status IN ('pending','approved')`, actor.AccountID, a.ID, now, actor.AccountID).Error
}
func phoneChangeError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{personalErrors.ErrInvalidPhoneChange, personalErrors.ErrPhoneChangeRetry, personalErrors.ErrRecoveryPhoneUnavailable, personalErrors.ErrInvalidProof, personalErrors.ErrSessionRevoked, personalErrors.ErrAuthSessionScope, personalErrors.ErrAuthAttemptNotFound, shared.ErrPersonalScope, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	return shared.NewUnexpectedError(errors.New("no se pudo consultar o completar el cambio de teléfono"))
}
