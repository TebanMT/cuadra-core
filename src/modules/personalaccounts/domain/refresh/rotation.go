package refresh

import (
	"crypto/subtle"
	"math"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	"github.com/google/uuid"
)

const ResponseTTL = 120 * time.Second
const Purpose = "refresh"

// Generation is the consumed generation, not the newly issued generation.
// History remains after ciphertext cleanup to prevent reusing consumed tokens.
type Rotation struct {
	AccountID, SessionID         uuid.UUID
	Generation                   int64
	OldRefreshHash               [32]byte
	OldRevocationTicketHash      *[32]byte
	OperationID                  uuid.UUID
	RequestHash                  [32]byte
	Response                     security.Envelope
	ResponseExpiresAt, CreatedAt time.Time
}

func NewRotation(previous *session.Session, operationID uuid.UUID, requestHash [32]byte, now time.Time) (*Rotation, error) {
	if previous == nil || !previous.ActiveAt(now) {
		return nil, personalErrors.ErrInvalidRefresh
	}
	ticket := previous.RevocationTicketHash
	at := now.UTC().Truncate(time.Millisecond)
	r := &Rotation{AccountID: previous.AccountID, SessionID: previous.ID, Generation: previous.Generation, OldRefreshHash: previous.RefreshHash, OldRevocationTicketHash: &ticket, OperationID: operationID, RequestHash: requestHash, CreatedAt: at, ResponseExpiresAt: at.Add(ResponseTTL)}
	return r, r.Validate()
}
func (r *Rotation) Validate() error {
	if r == nil || r.AccountID == uuid.Nil || r.SessionID == uuid.Nil || r.OperationID == uuid.Nil || r.Generation < 1 || r.Generation == math.MaxInt64 || r.OldRefreshHash == [32]byte{} || r.RequestHash == [32]byte{} || r.CreatedAt.IsZero() || !r.ResponseExpiresAt.Equal(r.CreatedAt.Add(ResponseTTL)) ||
		(r.OldRevocationTicketHash != nil && (*r.OldRevocationTicketHash == [32]byte{} || *r.OldRevocationTicketHash == r.OldRefreshHash)) ||
		(len(r.Response.Ciphertext) == 0) != (r.Response.KeyVersion == "") || (len(r.Response.Ciphertext) > 0 && len(r.Response.Ciphertext) < 29) {
		return personalErrors.ErrInvalidRefresh
	}
	return nil
}
func (r *Rotation) Context() security.ReceiptContext {
	return security.ReceiptContext{AccountID: r.AccountID, OperationID: r.OperationID, Purpose: Purpose}
}
func (r *Rotation) SetResponse(e security.Envelope) error {
	if r.Validate() != nil || e.KeyVersion == "" || len(e.Ciphertext) < 29 {
		return personalErrors.ErrInvalidRefresh
	}
	r.Response = security.Envelope{KeyVersion: e.KeyVersion, Ciphertext: append([]byte(nil), e.Ciphertext...)}
	return nil
}
func (r *Rotation) Match(operationID uuid.UUID, requestHash [32]byte) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.OperationID != operationID || subtle.ConstantTimeCompare(r.RequestHash[:], requestHash[:]) != 1 {
		return personalErrors.ErrInvalidProof
	}
	return nil
}
func (r *Rotation) ReadResponse(now time.Time) (security.Envelope, error) {
	if err := r.Validate(); err != nil {
		return security.Envelope{}, err
	}
	if now.IsZero() || now.Before(r.CreatedAt) || !now.Before(r.ResponseExpiresAt) || len(r.Response.Ciphertext) == 0 {
		return security.Envelope{}, personalErrors.ErrResultExpired
	}
	return security.Envelope{KeyVersion: r.Response.KeyVersion, Ciphertext: append([]byte(nil), r.Response.Ciphertext...)}, nil
}
func (Rotation) String() string     { return "personal session rotation (credentials redacted)" }
func (r Rotation) GoString() string { return r.String() }
