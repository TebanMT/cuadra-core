// Package phonechange binds a change of phone to the original active session.
package phonechange

import (
	"crypto/sha256"
	"crypto/subtle"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/google/uuid"
)

const StartPurpose = "phone_change_start"
const CompletePurpose = "phone_change_complete"

func ActorDigest(actor security.AccessClaims, attemptID, operationID uuid.UUID) [32]byte {
	h := sha256.New()
	h.Write([]byte("mancuerna:phone-change:actor:v1\x00"))
	for _, id := range []uuid.UUID{actor.AccountID, actor.SessionID, actor.DeviceID, attemptID, operationID} {
		h.Write(id[:])
	}
	return [32]byte(h.Sum(nil))
}

func ValidateActor(a *attempt.AuthAttempt, actor security.AccessClaims) error {
	if a == nil || a.Validate() != nil || actor.AccountID == uuid.Nil || actor.SessionID == uuid.Nil || actor.DeviceID == uuid.Nil || a.Purpose != attempt.PhoneChange || a.AccountID == nil || *a.AccountID != actor.AccountID || a.ProposedAccountID != actor.AccountID || a.Candidate.ID != actor.DeviceID {
		return personalErrors.ErrInvalidPhoneChange
	}
	return nil
}

func MatchInitiator(a *attempt.AuthAttempt, r *receipt.Receipt, actor security.AccessClaims) error {
	if ValidateActor(a, actor) != nil || r == nil || r.Match(StartPurpose, a.RequestHash) != nil || r.Owner != (receipt.Owner{AccountID: actor.AccountID, AttemptID: a.ID}) || r.ResultReference != actor.SessionID {
		return personalErrors.ErrInvalidPhoneChange
	}
	want := ActorDigest(actor, a.ID, r.OperationID)
	if subtle.ConstantTimeCompare(r.ResultSecretHash, want[:]) != 1 {
		return personalErrors.ErrInvalidPhoneChange
	}
	return nil
}

// Observation is only an internal lock precondition, never a response value.
type Observation struct{ Phone string }

func (Observation) String() string     { return "phone change observation (phone redacted)" }
func (o Observation) GoString() string { return o.String() }
