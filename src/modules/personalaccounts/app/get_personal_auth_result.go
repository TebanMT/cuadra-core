package app

import (
	"context"
	"crypto/subtle"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/phonechange"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type PersonalAuthResult struct {
	Session         *SessionTokens
	RecoveryKit     *RecoveryKit
	Acknowledgement *Acknowledgement
}

func (PersonalAuthResult) String() string     { return "personal auth result (credentials redacted)" }
func (r PersonalAuthResult) GoString() string { return r.String() }

type GetPersonalAuthResultDependencies struct {
	UoW             shared.UnitOfWork
	Sessions        repository.AuthSessionRepository
	Results         repository.PersonalAuthResultRepository
	Recovery        repository.RecoveryKeyRepository
	RecoverySecrets security.RecoveryKeys
	Envelopes       security.Envelopes
	Now             func() time.Time
}
type GetPersonalAuthResult struct {
	dependencies GetPersonalAuthResultDependencies
}

func NewGetPersonalAuthResult(d GetPersonalAuthResultDependencies) (*GetPersonalAuthResult, error) {
	if d.UoW == nil || d.Sessions == nil || d.Results == nil || d.Recovery == nil || d.RecoverySecrets == nil || d.Envelopes == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &GetPersonalAuthResult{d}, nil
}

// Execute only reads already committed effects. Neither an attempt secret nor
// a deletion result secret is accepted by this account-scoped capability.
func (u *GetPersonalAuthResult) Execute(ctx context.Context, actor security.AccessClaims, op uuid.UUID) (PersonalAuthResult, error) {
	if !validPersonalActor(actor) {
		return PersonalAuthResult{}, personalErrors.ErrInvalidAccessToken
	}
	if op == uuid.Nil {
		return PersonalAuthResult{}, personalErrors.ErrReceiptNotFound
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return PersonalAuthResult{}, err
	}
	d := u.dependencies
	var out PersonalAuthResult
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if e := requirePersonalSession(d.Sessions, tx, actor); e != nil {
			return e
		}
		stored, e := d.Results.GetAccountResult(tx, actor.AccountID, op)
		if e != nil {
			return e
		}
		r := stored.Receipt
		if r == nil || r.Validate() != nil || r.Owner.AccountID != actor.AccountID || r.OperationID != op {
			return personalErrors.ErrReceiptNotFound
		}
		switch r.Purpose {
		case devicesRevocationPurpose:
			if r.Owner.AttemptID != uuid.Nil || r.ResultReference == uuid.Nil || r.ResultReference.Variant() != uuid.RFC4122 || r.ResultReference.Version() < 1 || r.ResultReference.Version() > 8 || len(r.Response.Ciphertext) != 0 || len(r.ResultSecretHash) != 0 {
				return personalErrors.ErrReceiptNotFound
			}
			// This acknowledgement contains no credentials. The caller's live
			// session is checked above; the historical initiator may be revoked.
			ack := recoveryAcknowledgement(r)
			out.Acknowledgement = &ack
		case phonechange.CompletePurpose:
			if !stored.PhoneChangeCommitted || r.Owner.AttemptID == uuid.Nil || r.ResultReference != actor.SessionID {
				return personalErrors.ErrReceiptNotFound
			}
			digest := phonechange.ActorDigest(actor, r.Owner.AttemptID, op)
			if subtle.ConstantTimeCompare(r.ResultSecretHash, digest[:]) != 1 {
				return personalErrors.ErrInvalidProof
			}
			tokens, e := readPhoneChangeTokens(d.Sessions, d.Envelopes, d.Now, tx, actor, r.Owner.AttemptID, r)
			if e != nil {
				return e
			}
			out.Session = &tokens
		case "recovery_key_rotate":
			if r.Owner.AttemptID != uuid.Nil || r.ResultReference == uuid.Nil {
				return personalErrors.ErrReceiptNotFound
			}
			kit, e := readRecoveryRotationResponse(d.Recovery, d.RecoverySecrets, d.Envelopes, d.Now, tx, actor, r)
			if e != nil {
				return e
			}
			out.RecoveryKit = &kit
		default:
			return personalErrors.ErrReceiptNotFound
		}
		return nil
	})
	if err != nil {
		return PersonalAuthResult{}, err
	}
	return out, nil
}
