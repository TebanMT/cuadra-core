package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/phonechange"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/refresh"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type CompletePhoneChangeInput struct {
	Actor                    security.AccessClaims
	OperationID              uuid.UUID
	RequestHash              [32]byte
	Binding                  attempt.Binding
	Code, Origin, ProofToken string
}

func (CompletePhoneChangeInput) String() string {
	return "phone change completion (credentials redacted)"
}
func (i CompletePhoneChangeInput) GoString() string { return i.String() }

type CompletePhoneChange struct {
	dependencies PhoneChangeDependencies
	prepared     shared.PersonalAuthPreparedUnitOfWork
}

func NewCompletePhoneChange(d PhoneChangeDependencies) (*CompletePhoneChange, error) {
	p, err := validatePhoneChangeDependencies(d)
	if err != nil {
		return nil, err
	}
	return &CompletePhoneChange{d, p}, nil
}

func (u *CompletePhoneChange) Execute(ctx context.Context, in CompletePhoneChangeInput) (SessionTokens, error) {
	op := phoneOperation{OperationID: in.OperationID, Binding: in.Binding, RequestHash: in.RequestHash, Origin: in.Origin}
	if op.validate() != nil || len(in.Code) < 4 || len(in.Code) > 10 {
		return SessionTokens{}, personalErrors.ErrInvalidPhoneChange
	}
	for _, c := range in.Code {
		if c < '0' || c > '9' {
			return SessionTokens{}, personalErrors.ErrInvalidPhoneChange
		}
	}
	if !validRefreshSecret(in.ProofToken) {
		return SessionTokens{}, personalErrors.ErrInvalidProof
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, in.Actor.AccountID)
	if err != nil {
		return SessionTokens{}, err
	}
	d := u.dependencies
	current, token, reserved, err := u.reserve(ctx, in, op)
	if err != nil {
		return SessionTokens{}, err
	}
	if reserved {
		result, providerErr := d.Phone.Provider.Check(ctx, verification.CheckRequest{Binding: verification.Binding{Reference: current.ProviderReference, DestinationE164: current.DestinationE164}, Code: in.Code})
		if providerErr != nil {
			return SessionTokens{}, personalErrors.ErrAuthAttemptUncertain
		}
		current, err = persistPhoneChangeOutcome(ctx, d, in.Actor, in.Binding, token, result)
		if err != nil {
			return SessionTokens{}, err
		}
	}
	if current.State != attempt.Verified && current.State != attempt.Consumed {
		return SessionTokens{}, phoneChangePendingError(current)
	}
	return u.finalize(ctx, in, op)
}

func (u *CompletePhoneChange) reserve(ctx context.Context, in CompletePhoneChangeInput, op phoneOperation) (current *attempt.AuthAttempt, token attempt.OperationToken, reserved bool, err error) {
	d := u.dependencies
	for retry := 0; retry < 3; retry++ {
		found := false
		err = u.prepared.PreparedCommand(ctx, func(tx shared.Transaction) error {
			a, e := phoneChangeActor(d, tx, in.Actor, in.Binding, false)
			if e != nil {
				return e
			}
			current = a
			if _, e = phoneChangeReceipt(d, tx, in.Actor, op, phonechange.CompletePurpose); e == nil {
				found = true
				return nil
			} else if !errors.Is(e, personalErrors.ErrReceiptNotFound) {
				return e
			}
			if a.State != attempt.Pending {
				return phoneChangePendingError(a)
			}
			if e = reservePhoneBudget(d.Phone, tx, a, in.Origin, budget.Check, d.Phone.Now()); e != nil {
				if _, winner := phoneChangeReceipt(d, tx, in.Actor, op, phonechange.CompletePurpose); winner == nil {
					return errPhoneOperationReplay
				}
				return e
			}
			return nil
		}, func(tx shared.Transaction) error {
			a, e := phoneChangeActor(d, tx, in.Actor, in.Binding, true)
			if e != nil {
				return e
			}
			current = a
			if _, e = phoneChangeReceipt(d, tx, in.Actor, op, phonechange.CompletePurpose); e == nil {
				if !found {
					return errPhoneOperationReplay
				}
				return nil
			} else if !errors.Is(e, personalErrors.ErrReceiptNotFound) {
				return e
			}
			if found {
				return personalErrors.ErrInvalidPhoneChange
			}
			now := d.Phone.Now().UTC().Truncate(time.Millisecond)
			if e = d.Identity.CheckProof(tx, in.Actor, sha256.Sum256([]byte(in.ProofToken)), in.RequestHash, in.OperationID, now); e != nil {
				return e
			}
			revision := a.Revision
			token, e = a.ReserveCheck(in.OperationID, in.RequestHash, now, d.MaxChecks)
			if e != nil {
				return e
			}
			if e = d.Phone.Attempts.Save(tx, in.Binding, a, revision); e != nil {
				return e
			}
			r, e := newPhoneChangeReceipt(in.Actor, op, phonechange.CompletePurpose, now)
			if e != nil {
				return e
			}
			if e = d.Phone.Receipts.Create(tx, r); e != nil {
				return e
			}
			reserved = true
			return nil
		})
		if !errors.Is(err, errPhoneOperationReplay) {
			return
		}
	}
	return
}

func (u *CompletePhoneChange) finalize(ctx context.Context, in CompletePhoneChangeInput, op phoneOperation) (SessionTokens, error) {
	d := u.dependencies
	var result SessionTokens
	var err error
	for retry := 0; retry < 3; retry++ {
		var observation phonechange.Observation
		err = u.prepared.PreparedCommand(ctx, func(tx shared.Transaction) error {
			a, e := phoneChangeActor(d, tx, in.Actor, in.Binding, false)
			if e != nil {
				return e
			}
			if _, e = phoneChangeReceipt(d, tx, in.Actor, op, phonechange.CompletePurpose); e != nil {
				return e
			}
			if a.State == attempt.Consumed {
				return nil
			}
			if a.State != attempt.Verified {
				return phoneChangePendingError(a)
			}
			observation, e = d.Identity.LockPhones(tx, in.Actor, a.DestinationE164)
			return e
		}, func(tx shared.Transaction) error {
			a, e := phoneChangeActor(d, tx, in.Actor, in.Binding, true)
			if e != nil {
				return e
			}
			r, e := phoneChangeReceipt(d, tx, in.Actor, op, phonechange.CompletePurpose)
			if e != nil {
				return e
			}
			if a.State == attempt.Consumed {
				result, e = readPhoneChangeResponse(d, tx, in.Actor, a, r)
				return e
			}
			if a.State != attempt.Verified || a.ProviderOperation == nil || a.ProviderOperation.Kind != attempt.Check || a.ProviderOperation.ID != in.OperationID || a.ProviderOperation.RequestHash != in.RequestHash || a.VerifiedAt == nil || a.ProviderReference == "" || r.CompletedAt.Before(a.ProviderOperation.StartedAt) {
				return personalErrors.ErrInvalidPhoneChange
			}
			now := d.Phone.Now().UTC().Truncate(time.Millisecond)
			if !now.Before(a.ExpiresAt) {
				return personalErrors.ErrAuthAttemptExpired
			}
			if !now.Before(r.CompletedAt.Add(10 * time.Minute)) {
				return personalErrors.ErrResultExpired
			}
			proofHash := sha256.Sum256([]byte(in.ProofToken))
			if e = d.Proofs.Consume(tx, in.Actor, proofHash, security.PhoneChange, in.RequestHash, in.OperationID, now); e != nil {
				return e
			}
			if e = d.Identity.ValidatePhones(tx, in.Actor, a.DestinationE164, observation); e != nil {
				return e
			}
			s, e := d.Sessions.GetSession(tx, in.Actor.AccountID, in.Actor.SessionID)
			if e != nil {
				return e
			}
			rotation, e := refresh.NewRotation(s, in.OperationID, in.RequestHash, now)
			if e != nil {
				return e
			}
			next, e := randomAccessSecret()
			if e != nil {
				return e
			}
			ticket, e := randomAccessSecret()
			if e != nil {
				return e
			}
			if e = s.Rotate(sha256.Sum256([]byte(next)), sha256.Sum256([]byte(ticket)), now); e != nil {
				return e
			}
			if e = d.Identity.Apply(tx, in.Actor, in.Binding, observation, s, rotation, proofHash, now); e != nil {
				return e
			}
			revision := a.Revision
			if e = a.Consume(in.OperationID, now); e != nil {
				return e
			}
			if e = d.Phone.Attempts.Save(tx, in.Binding, a, revision); e != nil {
				return e
			}
			access, claims, e := d.Tokens.Issue(in.Actor.AccountID, in.Actor.SessionID, in.Actor.DeviceID, now)
			if e != nil {
				return e
			}
			result = SessionTokens{AccountID: in.Actor.AccountID.String(), SessionID: in.Actor.SessionID.String(), DeviceID: in.Actor.DeviceID.String(), AccessToken: access, AccessExpiresAt: accessTimestamp(claims.ExpiresAt), RefreshToken: next, RefreshExpiresAt: accessTimestamp(s.ExpiresAt), Generation: strconv.FormatInt(s.Generation, 10), RevocationTicket: ticket}
			plain, e := json.Marshal(phoneChangeResponse{SessionID: in.Actor.SessionID, DeviceID: in.Actor.DeviceID, AttemptID: a.ID, Tokens: result})
			if e != nil {
				return personalErrors.ErrInvalidEnvelope
			}
			defer clear(plain)
			envelope, e := d.Envelopes.SealReceipt(r.Context(), plain)
			if e != nil {
				return e
			}
			if e = r.SetResponse(envelope, r.CompletedAt.Add(10*time.Minute), in.Actor.SessionID); e != nil {
				return e
			}
			if e = d.Phone.Receipts.SaveResponse(tx, r); e != nil {
				return e
			}
			return d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: in.Actor.AccountID, ActorDeviceID: in.Actor.DeviceID, OperationID: in.OperationID, Type: "phone_changed", OccurredAt: now})
		})
		if !errors.Is(err, personalErrors.ErrPhoneChangeRetry) {
			break
		}
	}
	if err != nil {
		return SessionTokens{}, err
	}
	return result, nil
}
