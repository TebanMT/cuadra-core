// Package deletion models acceptance of a deletion request, never physical
// purge or release of a telephone. Retention values have no code defaults.
package deletion

import (
	"crypto/sha256"
	"encoding/base64"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/google/uuid"
)

const Purpose = "delete_account"
const Confirmation = "delete_my_account"

type Policy struct {
	Version    string
	PurgeDelay time.Duration
}

func (p *Policy) Validate() error {
	if p == nil || !utf8.ValidString(p.Version) || len([]rune(p.Version)) < 1 || len([]rune(p.Version)) > 80 || strings.TrimSpace(p.Version) != p.Version || strings.ContainsRune(p.Version, 0) || p.PurgeDelay < time.Millisecond || p.PurgeDelay%time.Millisecond != 0 {
		return personalErrors.ErrDeletionPolicyNotConfigured
	}
	return nil
}

type State struct {
	AccountID       uuid.UUID
	Status          string
	SecurityVersion int64
	RequestedAt     *time.Time
	PolicyVersion   string
	PurgeAfter      *time.Time
	DeletedAt       *time.Time
}

func (s *State) Request(policy *Policy, now time.Time) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if s == nil || !validID(s.AccountID) || s.Status != "active" || s.DeletedAt != nil || s.RequestedAt != nil || s.PurgeAfter != nil || s.PolicyVersion != "" || s.SecurityVersion < 1 || s.SecurityVersion == math.MaxInt64 {
		return personalErrors.ErrDeletionState
	}
	at := now.UTC().Truncate(time.Millisecond)
	purge := at.Add(policy.PurgeDelay)
	if !validTime(at) || !validTime(purge) || !purge.After(at) {
		return personalErrors.ErrInvalidDeletion
	}
	s.Status, s.SecurityVersion, s.PolicyVersion = "deletion_pending", s.SecurityVersion+1, policy.Version
	s.RequestedAt, s.PurgeAfter = &at, &purge
	return nil
}

type Request struct {
	OperationID uuid.UUID
	RequestHash [32]byte
	ProofHash   [32]byte
	Policy      Policy
	RequestedAt time.Time
}

func (r Request) Validate() error {
	if err := r.Policy.Validate(); err != nil {
		return err
	}
	if !validID(r.OperationID) || r.RequestHash == [32]byte{} || r.ProofHash == [32]byte{} || !validTime(r.RequestedAt) {
		return personalErrors.ErrInvalidDeletion
	}
	return nil
}

type Result struct {
	OperationID uuid.UUID
	CompletedAt time.Time
}

func (r Result) Validate() error {
	if !validID(r.OperationID) || !validTime(r.CompletedAt) {
		return personalErrors.ErrDeletionResultNotFound
	}
	return nil
}

// ResultHash binds the limited capability to one operation. Reusing a secret
// for another account's operation cannot produce an ambiguous identity lookup.
// The client generates and durably saves 32 random bytes before the request.
func ResultHash(operationID uuid.UUID, secret string) ([32]byte, error) {
	if !validID(operationID) || len(secret) != 43 {
		return [32]byte{}, personalErrors.ErrInvalidDeletion
	}
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != secret {
		return [32]byte{}, personalErrors.ErrInvalidDeletion
	}
	defer clear(decoded)
	h := sha256.New()
	h.Write([]byte("mancuerna:deletion-result:v1\x00"))
	h.Write(operationID[:])
	h.Write(decoded)
	return [32]byte(h.Sum(nil)), nil
}

func validID(id uuid.UUID) bool {
	return id != uuid.Nil && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
}
func validTime(at time.Time) bool {
	return !at.IsZero() && at.UTC().Year() >= 1 && at.UTC().Year() <= 9999
}
