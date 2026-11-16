// Package vault models only the server's encrypted copy of a local vault key.
// A usable data key belongs to a short-lived application response, never here.
package vault

import (
	"strings"
	"time"
	"unicode/utf8"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/google/uuid"
)

type Vault struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	Envelope  security.Envelope
	CreatedAt time.Time
	RetiredAt *time.Time
}

func New(accountID, id uuid.UUID, envelope security.Envelope, now time.Time) (*Vault, error) {
	v := &Vault{ID: id, AccountID: accountID, Envelope: security.Envelope{KeyVersion: envelope.KeyVersion, Ciphertext: append([]byte(nil), envelope.Ciphertext...)}, CreatedAt: now.UTC().Truncate(time.Microsecond)}
	return v, v.Validate()
}

func (Vault) String() string     { return "personal vault (envelope redacted)" }
func (v Vault) GoString() string { return v.String() }

func validID(id uuid.UUID) bool {
	return id != uuid.Nil && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
}

func (v *Vault) Validate() error {
	// The HTTP key_version is limited to 80 characters. Bound opaque envelopes
	// without coupling the entity to a particular AEAD wire format.
	if v == nil || !validID(v.ID) || !validID(v.AccountID) || v.CreatedAt.IsZero() || len(v.Envelope.Ciphertext) < 29 || len(v.Envelope.Ciphertext) > 4096 ||
		v.Envelope.KeyVersion == "" || len(v.Envelope.KeyVersion) > 80 || strings.TrimSpace(v.Envelope.KeyVersion) != v.Envelope.KeyVersion || !utf8.ValidString(v.Envelope.KeyVersion) || strings.ContainsRune(v.Envelope.KeyVersion, 0) {
		return personalErrors.ErrVaultState
	}
	if v.RetiredAt != nil && (v.RetiredAt.IsZero() || v.RetiredAt.Before(v.CreatedAt)) {
		return personalErrors.ErrVaultState
	}
	return nil
}

func (v *Vault) Live() bool { return v.Validate() == nil && v.RetiredAt == nil }

func (v *Vault) Context() security.VaultContext {
	return security.VaultContext{AccountID: v.AccountID, VaultID: v.ID}
}
