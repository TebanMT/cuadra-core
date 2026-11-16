package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/phonechange"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type StartPhoneChangeInput struct {
	Actor              security.AccessClaims
	OperationID        uuid.UUID
	RequestHash        [32]byte
	Binding            attempt.Binding
	DestinationE164    string
	Channel            verification.Channel
	Origin, ProofToken string
}

func (StartPhoneChangeInput) String() string {
	return "phone change start (credentials and destination redacted)"
}
func (i StartPhoneChangeInput) GoString() string { return i.String() }

type StartPhoneChange struct {
	dependencies PhoneChangeDependencies
	prepared     shared.PersonalAuthPreparedUnitOfWork
}

func NewStartPhoneChange(d PhoneChangeDependencies) (*StartPhoneChange, error) {
	p, err := validatePhoneChangeDependencies(d)
	if err != nil {
		return nil, err
	}
	return &StartPhoneChange{d, p}, nil
}

func (u *StartPhoneChange) Execute(ctx context.Context, in StartPhoneChangeInput) (*attempt.AuthAttempt, error) {
	d := u.dependencies
	op := phoneOperation{OperationID: in.OperationID, Binding: in.Binding, RequestHash: in.RequestHash, Origin: in.Origin}
	if op.validate() != nil {
		return nil, personalErrors.ErrInvalidPhoneChange
	}
	if !validRefreshSecret(in.ProofToken) {
		return nil, personalErrors.ErrInvalidProof
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, in.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	var current *attempt.AuthAttempt
	var token attempt.OperationToken
	reserved := false
	for retry := 0; retry < 3; retry++ {
		var observation phonechange.Observation
		found := false
		err = u.prepared.PreparedCommand(ctx, func(tx shared.Transaction) error {
			if err := requirePersonalSession(d.Sessions, tx, in.Actor); err != nil {
				return err
			}
			a, e := d.Phone.Attempts.Get(tx, in.Binding)
			if e == nil {
				current = a
				found = true
				return nil
			}
			if !errors.Is(e, personalErrors.ErrAuthAttemptNotFound) {
				return e
			}
			phone, e := d.Identity.CurrentPhone(tx, in.Actor)
			if e != nil {
				return e
			}
			if phone == in.DestinationE164 {
				return personalErrors.ErrInvalidPhoneChange
			}
			// Constructing the aggregate validates the destination/channel before
			// reserving any provider capacity. Candidate comes from the actor.
			device, e := d.Sessions.GetDevice(tx, in.Actor.AccountID, in.Actor.DeviceID)
			if e != nil {
				return e
			}
			var nonce [32]byte
			if _, e = rand.Read(nonce[:]); e != nil {
				return personalErrors.ErrVerificationUnavailable
			}
			current, e = attempt.New(attempt.NewAttempt{Binding: in.Binding, AccountID: &in.Actor.AccountID, ProposedAccountID: in.Actor.AccountID, Candidate: attempt.Candidate{ID: device.ID, PublicKey: device.PublicKey, Platform: device.Platform, Label: device.Label}, Nonce: nonce, Purpose: attempt.PhoneChange, DestinationE164: in.DestinationE164, Channel: in.Channel, RequestHash: in.RequestHash}, d.Phone.Now(), d.Phone.ProviderValidity)
			if e != nil {
				return e
			}
			if e = reservePhoneBudget(d.Phone, tx, current, in.Origin, budget.Send, d.Phone.Now()); e != nil {
				if _, winner := d.Phone.Attempts.Get(tx, in.Binding); winner == nil {
					return errPhoneOperationReplay
				}
				return e
			}
			observation, e = d.Identity.LockPhones(tx, in.Actor, in.DestinationE164)
			return e
		}, func(tx shared.Transaction) error {
			if e := requirePersonalSession(d.Sessions, tx, in.Actor); e != nil {
				return e
			}
			if found {
				a, e := phoneChangeActor(d, tx, in.Actor, in.Binding, false)
				if e != nil {
					return e
				}
				if _, e = phoneChangeReceipt(d, tx, in.Actor, op, phonechange.StartPurpose); e != nil {
					return e
				}
				if a.RequestHash != in.RequestHash || a.DestinationE164 != in.DestinationE164 {
					return personalErrors.ErrIdempotencyMismatch
				}
				current = a
				return nil
			}
			if _, e := d.Phone.Attempts.Get(tx, in.Binding); e == nil {
				return errPhoneOperationReplay
			} else if !errors.Is(e, personalErrors.ErrAuthAttemptNotFound) {
				return e
			}
			now := d.Phone.Now().UTC().Truncate(time.Millisecond)
			if e := d.Proofs.Consume(tx, in.Actor, sha256.Sum256([]byte(in.ProofToken)), security.PhoneChange, in.RequestHash, in.OperationID, now); e != nil {
				return e
			}
			if e := d.Identity.ValidatePhones(tx, in.Actor, in.DestinationE164, observation); e != nil {
				return e
			}
			if e := d.Phone.Attempts.Create(tx, current); e != nil {
				return e
			}
			revision := current.Revision
			var e error
			token, e = current.ReserveSend(in.OperationID, in.RequestHash, in.Channel, now)
			if e != nil {
				return e
			}
			if e = d.Phone.Attempts.Save(tx, in.Binding, current, revision); e != nil {
				return e
			}
			r, e := newPhoneChangeReceipt(in.Actor, op, phonechange.StartPurpose, now)
			if e != nil {
				return e
			}
			if e = d.Phone.Receipts.Create(tx, r); e != nil {
				return e
			}
			if e = d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: in.Actor.AccountID, ActorDeviceID: in.Actor.DeviceID, OperationID: in.OperationID, Type: "phone_change_requested", OccurredAt: now}); e != nil {
				return e
			}
			reserved = true
			return nil
		})
		if !errors.Is(err, errPhoneOperationReplay) && !errors.Is(err, personalErrors.ErrPhoneChangeRetry) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if !reserved {
		return current, nil
	}
	result, providerErr := d.Phone.Provider.Start(ctx, verification.StartRequest{DestinationE164: current.DestinationE164, Channel: current.Channel})
	if providerErr != nil {
		return current, nil
	}
	return persistPhoneChangeOutcome(ctx, d, in.Actor, in.Binding, token, result)
}
