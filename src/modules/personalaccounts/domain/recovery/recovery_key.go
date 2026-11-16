package recovery

import (
	"bytes"
	"strings"
	"time"
	"unicode/utf8"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/google/uuid"
)

// RecoveryKey never contains the bearer secret. Its verifier is versioned so
// old server keys remain usable until the associated kits have been retired.
type RecoveryKey struct {
	ID         uuid.UUID
	AccountID  uuid.UUID
	Verifier   security.RecoveryVerifier
	Generation int64
	CreatedAt  time.Time
	PreparedAt *time.Time
	ConsumedAt *time.Time
	RevokedAt  *time.Time
}

func NewKey(accountID, keyID uuid.UUID, generation int64, verifier security.RecoveryVerifier, now time.Time) (*RecoveryKey, error) {
	k := &RecoveryKey{ID: keyID, AccountID: accountID, Generation: generation, Verifier: security.RecoveryVerifier{KeyVersion: verifier.KeyVersion, Digest: append([]byte(nil), verifier.Digest...)}, CreatedAt: now.UTC().Truncate(time.Microsecond)}
	return k, k.Validate()
}

func (RecoveryKey) String() string     { return "recovery key (verifier redacted)" }
func (k RecoveryKey) GoString() string { return k.String() }

func validID(id uuid.UUID) bool {
	return id != uuid.Nil && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
}

func (k *RecoveryKey) Validate() error {
	if k == nil || !validID(k.ID) || !validID(k.AccountID) || k.Generation < 1 || k.CreatedAt.IsZero() || len(k.Verifier.Digest) != 32 || bytes.Equal(k.Verifier.Digest, make([]byte, 32)) ||
		k.Verifier.KeyVersion == "" || len(k.Verifier.KeyVersion) > 128 || strings.TrimSpace(k.Verifier.KeyVersion) != k.Verifier.KeyVersion || !utf8.ValidString(k.Verifier.KeyVersion) || strings.ContainsRune(k.Verifier.KeyVersion, 0) {
		return personalErrors.ErrInvalidRecoveryKey
	}
	for _, stamp := range []*time.Time{k.PreparedAt, k.ConsumedAt, k.RevokedAt} {
		if stamp != nil && (stamp.IsZero() || stamp.Before(k.CreatedAt)) {
			return personalErrors.ErrInvalidRecoveryKey
		}
	}
	if (k.ConsumedAt != nil && k.RevokedAt != nil) || (k.PreparedAt != nil && ((k.ConsumedAt != nil && k.ConsumedAt.Before(*k.PreparedAt)) || (k.RevokedAt != nil && k.RevokedAt.Before(*k.PreparedAt)))) {
		return personalErrors.ErrInvalidRecoveryKey
	}
	return nil
}

func (k *RecoveryKey) Live() bool {
	return k.Validate() == nil && k.ConsumedAt == nil && k.RevokedAt == nil
}

func (k *RecoveryKey) Prepared() bool { return k.Live() && k.PreparedAt != nil }

func (k *RecoveryKey) stateTime(now time.Time) error {
	if err := k.Validate(); err != nil {
		return err
	}
	if now.IsZero() || now.Before(k.CreatedAt) || (k.PreparedAt != nil && now.Before(*k.PreparedAt)) {
		return personalErrors.ErrInvalidRecoveryKey
	}
	return nil
}

func (k *RecoveryKey) ConfirmSaved(now time.Time) error {
	if err := k.stateTime(now); err != nil {
		return err
	}
	if !k.Live() {
		return personalErrors.ErrRecoveryKeyState
	}
	if k.PreparedAt == nil {
		stamp := now.UTC().Truncate(time.Microsecond)
		k.PreparedAt = &stamp
	}
	return nil
}

// Possession of a valid kit is verified separately before consuming. The UI
// confirmation is honest preparation metadata, not cryptographic validity.
// Replayed successful operations recover their receipt rather than consuming
// an already-consumed key a second time.
func (k *RecoveryKey) Consume(now time.Time) error {
	if err := k.stateTime(now); err != nil {
		return err
	}
	if !k.Live() {
		return personalErrors.ErrRecoveryKeyState
	}
	stamp := now.UTC().Truncate(time.Microsecond)
	k.ConsumedAt = &stamp
	return nil
}

func (k *RecoveryKey) Revoke(now time.Time) error {
	if err := k.stateTime(now); err != nil {
		return err
	}
	if k.ConsumedAt != nil {
		return personalErrors.ErrRecoveryKeyState
	}
	if k.RevokedAt == nil {
		stamp := now.UTC().Truncate(time.Microsecond)
		k.RevokedAt = &stamp
	}
	return nil
}
