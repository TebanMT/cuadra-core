// Package receipt stores durable operation identity separately from short-lived
// encrypted responses. Removing a response must never permit repeating effects.
package receipt

import (
	"crypto/subtle"
	"time"
	"unicode/utf8"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/google/uuid"
)

type Owner struct {
	AccountID uuid.UUID
	AttemptID uuid.UUID
}

func (o Owner) Valid() bool { return o.AccountID != uuid.Nil || o.AttemptID != uuid.Nil }

// Receipt contains no plaintext tokens, phone, OTP or recovery key. AccountID
// and AttemptID are immutable because both participate in the envelope AAD.
type Receipt struct {
	OperationID       uuid.UUID
	Owner             Owner
	Purpose           string
	RequestHash       [32]byte
	ResultReference   uuid.UUID
	Response          security.Envelope
	ResponseExpiresAt time.Time
	ResultSecretHash  []byte
	CompletedAt       time.Time
}

func New(operationID uuid.UUID, owner Owner, purpose string, hash [32]byte, now time.Time) (*Receipt, error) {
	at := now.UTC().Truncate(time.Millisecond)
	r := &Receipt{OperationID: operationID, Owner: owner, Purpose: purpose, RequestHash: hash, CompletedAt: at, ResponseExpiresAt: at}
	return r, r.Validate()
}

func (r *Receipt) Validate() error {
	if r == nil || r.OperationID == uuid.Nil || !r.Owner.Valid() || r.RequestHash == [32]byte{} ||
		len(r.Purpose) == 0 || len(r.Purpose) > 80 || !utf8.ValidString(r.Purpose) ||
		r.CompletedAt.IsZero() || r.ResponseExpiresAt.IsZero() ||
		(len(r.ResultSecretHash) != 0 && len(r.ResultSecretHash) != 32) ||
		(len(r.Response.Ciphertext) == 0) != (r.Response.KeyVersion == "") ||
		(len(r.Response.Ciphertext) > 0 && (len(r.Response.Ciphertext) < 29 || !r.ResponseExpiresAt.After(r.CompletedAt) || r.ResponseExpiresAt.After(r.CompletedAt.Add(10*time.Minute)))) {
		return personalErrors.ErrInvalidReceipt
	}
	return nil
}

func (r *Receipt) Context() security.ReceiptContext {
	return security.ReceiptContext{AccountID: r.Owner.AccountID, AttemptID: r.Owner.AttemptID, OperationID: r.OperationID, Purpose: r.Purpose}
}

func (r *Receipt) Match(purpose string, hash [32]byte) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Purpose != purpose || subtle.ConstantTimeCompare(r.RequestHash[:], hash[:]) != 1 {
		return personalErrors.ErrIdempotencyMismatch
	}
	return nil
}

// SetResponse only replaces the response for an already reserved effect. The
// caller encrypts with Context first; all update conditions remain immutable.
func (r *Receipt) SetResponse(response security.Envelope, expiresAt time.Time, reference uuid.UUID) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if len(response.Ciphertext) < 29 || response.KeyVersion == "" || expiresAt.IsZero() ||
		!expiresAt.After(r.CompletedAt) || expiresAt.After(r.CompletedAt.Add(10*time.Minute)) {
		return personalErrors.ErrInvalidReceipt
	}
	r.Response = security.Envelope{KeyVersion: response.KeyVersion, Ciphertext: append([]byte(nil), response.Ciphertext...)}
	r.ResponseExpiresAt = expiresAt.UTC()
	r.ResultReference = reference
	return nil
}

func (r *Receipt) ReadResponse(now time.Time) (security.Envelope, error) {
	if err := r.Validate(); err != nil {
		return security.Envelope{}, err
	}
	if now.IsZero() || !now.Before(r.ResponseExpiresAt) || len(r.Response.Ciphertext) == 0 {
		return security.Envelope{}, personalErrors.ErrResultExpired
	}
	return security.Envelope{KeyVersion: r.Response.KeyVersion, Ciphertext: append([]byte(nil), r.Response.Ciphertext...)}, nil
}
