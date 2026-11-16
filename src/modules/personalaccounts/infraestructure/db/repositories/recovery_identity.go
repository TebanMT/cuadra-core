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
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type RecoveryIdentityPostgresRepository struct{ phones *IdentityAccessRepository }

func NewRecoveryIdentityRepository(phoneHMACKey []byte) (*RecoveryIdentityPostgresRepository, error) {
	phones, err := NewIdentityAccessRepository(phoneHMACKey)
	if err != nil {
		return nil, err
	}
	return &RecoveryIdentityPostgresRepository{phones: phones}, nil
}
func (*RecoveryIdentityPostgresRepository) String() string {
	return "recovery identity repository (key redacted)"
}
func (r *RecoveryIdentityPostgresRepository) GoString() string { return r.String() }

var _ repository.RecoveryIdentityRepository = (*RecoveryIdentityPostgresRepository)(nil)

type recoveryChallenge struct {
	ID              uuid.UUID
	AccountID       *uuid.UUID
	DestinationE164 string
	VerifiedAt      *time.Time
	Usable          bool
}

func (recoveryChallenge) String() string     { return "recovery challenge (destination redacted)" }
func (c recoveryChallenge) GoString() string { return c.String() }

func readRecoveryChallenge(tx *gorm.DB, binding attempt.Binding, lock bool) (recoveryChallenge, error) {
	query := `SELECT c.id,c.account_id,c.destination_e164,c.verified_at,
 (c.purpose='recovery' AND c.status='verified' AND c.expires_at>clock_timestamp()
 AND c.verified_at IS NOT NULL AND c.verified_at>=c.created_at AND c.verified_at<=clock_timestamp()
 AND c.verified_at<c.expires_at AND c.consumed_at IS NULL AND c.provider_reference IS NOT NULL
 AND c.provider_reference<>'' AND c.provider_operation_kind='check'
 AND EXISTS(SELECT 1 FROM mancuerna.auth_receipts r WHERE r.operation_id=c.provider_operation_id
 AND r.attempt_id=c.id AND r.account_id IS NULL AND r.purpose='auth_check'
 AND r.request_hash=c.provider_operation_hash AND r.completed_at>=c.provider_operation_started_at
 AND r.completed_at<=clock_timestamp())) AS usable
 FROM mancuerna.auth_challenges c WHERE c.id=? AND c.continuation_hash=?`
	if lock {
		query += " FOR UPDATE OF c"
	}
	var c recoveryChallenge
	result := tx.Raw(query, binding.ID, binding.ContinuationHash[:]).Scan(&c)
	if result.Error != nil {
		return recoveryChallenge{}, result.Error
	}
	if result.RowsAffected != 1 || !c.Usable || c.VerifiedAt == nil || !accessPhoneFormat.MatchString(c.DestinationE164) {
		return recoveryChallenge{}, personalErrors.ErrRecoveryKeyState
	}
	return c, nil
}

func (r *RecoveryIdentityPostgresRepository) ResolveVerifiedRecovery(handle shared.Transaction, binding attempt.Binding, keyID uuid.UUID) (bound repository.BoundRecovery, err error) {
	defer func() {
		if err != nil {
			bound = repository.BoundRecovery{}
			err = recoveryIdentityError(err)
		}
	}()
	if r == nil || r.phones == nil || binding.Validate() != nil || keyID == uuid.Nil {
		return bound, personalErrors.ErrAccessIdentityScope
	}
	t, ok := handle.(*shared.GormTransaction)
	if !ok || t == nil || t.Tx == nil || t.Tx.Statement == nil {
		return bound, personalErrors.ErrAccessIdentityScope
	}
	if _, ok = t.Tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return bound, personalErrors.ErrAccessIdentityScope
	}
	tx := t.Tx.Session(&gorm.Session{Logger: logger.Discard})
	hash, err := accessAttemptScope(tx, binding.ID)
	if err != nil {
		return bound, err
	}
	if hash != binding.ContinuationHash {
		return bound, personalErrors.ErrAccessIdentityScope
	}
	// Prove direct OTP before allowing the narrow recovery-ID lookup. Neither
	// an access attempt nor an approved Fetch response can reach this lookup.
	before, err := readRecoveryChallenge(tx, binding, false)
	if err != nil {
		return bound, err
	}
	if before.AccountID != nil {
		return bound, personalErrors.ErrRecoveryKeyState
	}
	var resolved uuid.NullUUID
	if err = tx.Raw("SELECT mancuerna_private.resolve_identity('recovery',?)", keyID.String()).Row().Scan(&resolved); err != nil {
		return bound, err
	}
	if !resolved.Valid {
		return bound, personalErrors.ErrRecoveryKeyState
	}
	if err = tx.Exec("SELECT set_config('mancuerna.account_id',?,true)", resolved.UUID.String()).Error; err != nil {
		return bound, err
	}
	var currentPhone string
	err = tx.Raw("SELECT phone_e164 FROM mancuerna.accounts WHERE id=? AND status='active' AND deleted_at IS NULL", resolved.UUID).Row().Scan(&currentPhone)
	if errors.Is(err, sql.ErrNoRows) {
		return bound, personalErrors.ErrRecoveryKeyState
	}
	if err != nil {
		return bound, err
	}
	locks := []int64{r.phones.phoneLockKey(currentPhone), r.phones.phoneLockKey(before.DestinationE164)}
	sort.Slice(locks, func(i, j int) bool { return locks[i] < locks[j] })
	for i, key := range locks {
		if i > 0 && key == locks[i-1] {
			continue
		}
		if err = tx.Exec("SELECT pg_advisory_xact_lock(?)", key).Error; err != nil {
			return bound, err
		}
	}
	var locked uuid.UUID
	err = tx.Raw("UPDATE mancuerna.sync_streams SET last_sequence=last_sequence WHERE account_id=? RETURNING account_id", resolved.UUID).Row().Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return bound, personalErrors.ErrRecoveryKeyState
	}
	if err != nil {
		return bound, err
	}
	var afterPhone string
	err = tx.Raw("SELECT phone_e164 FROM mancuerna.accounts WHERE id=? AND status='active' AND deleted_at IS NULL", resolved.UUID).Row().Scan(&afterPhone)
	if errors.Is(err, sql.ErrNoRows) {
		return bound, personalErrors.ErrRecoveryKeyState
	}
	if err != nil {
		return bound, err
	}
	// Do not acquire a third phone lock after taking the account mutex. Restart
	// the whole transaction when the initial unlocked observation was stale.
	if afterPhone != currentPhone {
		return bound, personalErrors.ErrRecoveryIdentityRetry
	}
	after, err := readRecoveryChallenge(tx, binding, true)
	if err != nil {
		return bound, err
	}
	if after.AccountID != nil || after.DestinationE164 != before.DestinationE164 || !after.VerifiedAt.Equal(*before.VerifiedAt) {
		return bound, personalErrors.ErrRecoveryKeyState
	}
	var destination uuid.NullUUID
	if err = tx.Raw("SELECT mancuerna_private.resolve_identity('phone',?)", after.DestinationE164).Row().Scan(&destination); err != nil {
		return bound, err
	}
	return repository.BoundRecovery{AccountID: resolved.UUID, PhoneChangeRequired: after.DestinationE164 != afterPhone, PhoneAvailable: !destination.Valid || destination.UUID == resolved.UUID}, nil
}

func (r *RecoveryIdentityPostgresRepository) ApplyRecovery(handle shared.Transaction, binding attempt.Binding, accountID, keyID uuid.UUID, confirm bool, now time.Time) (err error) {
	defer func() { err = recoveryIdentityError(err) }()
	if r == nil || r.phones == nil || binding.Validate() != nil || keyID == uuid.Nil || now.IsZero() {
		return personalErrors.ErrRecoveryKeyState
	}
	tx, err := authSessionConnection(handle, accountID, true)
	if err != nil {
		return err
	}
	var scope bool
	err = tx.Raw(`SELECT current_setting('mancuerna.attempt_id',true)=? AND decode(current_setting('mancuerna.attempt_hash',true),'hex')=?`, binding.ID.String(), binding.ContinuationHash[:]).Scan(&scope).Error
	if err != nil {
		return err
	}
	if !scope {
		return personalErrors.ErrAccessIdentityScope
	}
	a, err := readRecoveryChallenge(tx, binding, true)
	if err != nil {
		return err
	}
	if a.AccountID != nil {
		return personalErrors.ErrRecoveryKeyState
	}
	var currentPhone string
	err = tx.Raw("SELECT phone_e164 FROM mancuerna.accounts WHERE id=? AND status='active' AND deleted_at IS NULL", accountID).Row().Scan(&currentPhone)
	if errors.Is(err, sql.ErrNoRows) {
		return personalErrors.ErrRecoveryKeyState
	}
	if err != nil {
		return err
	}
	if currentPhone != a.DestinationE164 && !confirm {
		return personalErrors.ErrRecoveryKeyState
	}
	// Apply cannot be used as a general account-scoped phone mutator. Require
	// the actual transaction locks acquired by Resolve before the account lock;
	// never acquire missing phone locks here in the reverse order.
	for _, phone := range []string{currentPhone, a.DestinationE164} {
		key := uint64(r.phones.phoneLockKey(phone))
		var held bool
		err = tx.Raw(`SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_locks WHERE locktype='advisory'
 AND pid=pg_backend_pid() AND mode='ExclusiveLock' AND granted AND objsubid=1
 AND classid::bigint=? AND objid::bigint=?)`, int64(key>>32), int64(key&0xffffffff)).Scan(&held).Error
		if err != nil {
			return err
		}
		if !held {
			return personalErrors.ErrAccessIdentityScope
		}
	}
	var available bool
	err = tx.Raw("SELECT coalesce(mancuerna_private.resolve_identity('phone',?)=?,true)", a.DestinationE164, accountID).Scan(&available).Error
	if err != nil {
		return err
	}
	if !available {
		return personalErrors.ErrRecoveryPhoneUnavailable
	}
	var consumed bool
	err = tx.Raw("SELECT EXISTS(SELECT 1 FROM mancuerna.recovery_keys WHERE account_id=? AND id=? AND consumed_at=? AND revoked_at IS NULL)", accountID, keyID, now.UTC()).Scan(&consumed).Error
	if err != nil {
		return err
	}
	if !consumed {
		return personalErrors.ErrRecoveryKeyState
	}
	result := tx.Exec(`UPDATE mancuerna.accounts SET phone_changed_at=CASE WHEN phone_e164<>? THEN ? ELSE phone_changed_at END,
 phone_e164=?,phone_verified_at=?,security_version=security_version+1,updated_at=?
 WHERE id=? AND status='active' AND deleted_at IS NULL AND security_version<9223372036854775807`, a.DestinationE164, now.UTC(), a.DestinationE164, a.VerifiedAt, now.UTC(), accountID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return personalErrors.ErrRecoveryKeyState
	}
	// Device authorization is preserved as history. Every previous refresh
	// family/session is withdrawn; a new candidate session is created later.
	return tx.Exec("UPDATE mancuerna.sessions SET revoked_at=? WHERE account_id=? AND revoked_at IS NULL", now.UTC(), accountID).Error
}

func recoveryIdentityError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{personalErrors.ErrRecoveryKeyState, personalErrors.ErrRecoveryPhoneUnavailable, personalErrors.ErrRecoveryIdentityRetry, personalErrors.ErrAccessIdentityScope, personalErrors.ErrAccessIdentityConfiguration, personalErrors.ErrAuthSessionScope, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	return shared.NewUnexpectedError(errors.New("no se pudo completar la identidad de recuperación"))
}
