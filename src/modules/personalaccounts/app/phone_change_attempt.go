package app

import (
	"context"
	"errors"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

type PhoneChangeAttemptDependencies struct {
	Phone    PhoneChangeDependencies
	Resolver repository.PhoneChangeAttemptResolver
	Policy   PhoneRetryPolicy
}
type GetPhoneChangeAttempt struct {
	dependencies PhoneChangeAttemptDependencies
}

func NewGetPhoneChangeAttempt(d PhoneChangeAttemptDependencies) (*GetPhoneChangeAttempt, error) {
	if _, err := validatePhoneChangeDependencies(d.Phone); err != nil {
		return nil, err
	}
	if d.Resolver == nil || d.Policy.ProviderTimeout <= 0 || d.Policy.ReconcileAfter < d.Policy.ProviderTimeout || d.Policy.ResendCooldown <= 0 {
		return nil, personalErrors.ErrVerificationConfiguration
	}
	return &GetPhoneChangeAttempt{d}, nil
}
func resolvePhoneChangeScope(ctx context.Context, d PhoneChangeAttemptDependencies, b attempt.Binding) (context.Context, security.AccessClaims, error) {
	if err := shared.ValidatePersonalAnonymousScope(ctx); err != nil {
		return nil, security.AccessClaims{}, err
	}
	if b.Validate() != nil {
		return nil, security.AccessClaims{}, personalErrors.ErrInvalidAuthAttempt
	}
	q, err := d.Phone.Phone.UoW.Query(shared.WithPersonalAttempt(ctx, b.ID, b.ContinuationHash))
	if err != nil {
		return nil, security.AccessClaims{}, err
	}
	actor, err := d.Resolver.Resolve(q, b)
	shared.CloseQuery(q)
	if err != nil {
		return nil, security.AccessClaims{}, err
	}
	// ctx is still the validated original anonymous context, not the scoped
	// query context. Never erase an inherited capability to make it pass.
	scoped, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	return scoped, actor, err
}
func readPhoneChangeAttempt(ctx context.Context, d PhoneChangeDependencies, actor security.AccessClaims, b attempt.Binding) (a *attempt.AuthAttempt, err error) {
	err = d.Phone.UoW.Command(ctx, func(tx shared.Transaction) error {
		var e error
		a, e = phoneChangeActor(d, tx, actor, b, true)
		if e != nil {
			return e
		}
		now := d.Phone.Now()
		if !now.Before(a.ExpiresAt) && (a.State == attempt.Pending || a.State == attempt.Uncertain || a.State == attempt.Verified) {
			revision := a.Revision
			if e = a.Expire(now); e != nil {
				return e
			}
			return d.Phone.Attempts.Save(tx, b, a, revision)
		}
		return nil
	})
	return
}

// Execute returns only the limited attempt view. Even consumed or verified
// phone attempts never return a session, kit, or authorization-required result.
func (u *GetPhoneChangeAttempt) Execute(ctx context.Context, b attempt.Binding) (AccessResult, error) {
	d := u.dependencies
	scoped, actor, err := resolvePhoneChangeScope(ctx, d, b)
	if err != nil {
		return AccessResult{}, err
	}
	a, err := readPhoneChangeAttempt(scoped, d.Phone, actor, b)
	if err != nil {
		return AccessResult{}, err
	}
	if a.State != attempt.Uncertain || a.ProviderReference == "" || a.ProviderOperation == nil || d.Phone.Phone.Now().Before(a.ProviderOperation.StartedAt.Add(d.Policy.ReconcileAfter)) {
		return AccessResult{Attempt: a}, nil
	}
	token := phoneToken(a)
	fetchCtx, cancel := context.WithTimeout(ctx, d.Policy.ProviderTimeout)
	result, providerErr := d.Phone.Phone.Provider.Fetch(fetchCtx, verification.Binding{Reference: a.ProviderReference, DestinationE164: a.DestinationE164})
	cancel()
	if providerErr != nil || result.Reference != a.ProviderReference || result.State == verification.Approved {
		a, err = readPhoneChangeAttempt(scoped, d.Phone, actor, b)
	} else {
		a, err = persistPhoneChangeOutcome(scoped, d.Phone, actor, b, token, result)
	}
	if err != nil {
		return AccessResult{}, err
	}
	return AccessResult{Attempt: a}, nil
}

type ResendPhoneChange struct {
	dependencies PhoneChangeAttemptDependencies
	get          *GetPhoneChangeAttempt
	prepared     shared.PersonalAuthPreparedUnitOfWork
}

func NewResendPhoneChange(d PhoneChangeAttemptDependencies) (*ResendPhoneChange, error) {
	get, err := NewGetPhoneChangeAttempt(d)
	if err != nil {
		return nil, err
	}
	prepared, err := validatePhoneChangeDependencies(d.Phone)
	if err != nil {
		return nil, err
	}
	return &ResendPhoneChange{d, get, prepared}, nil
}
func (u *ResendPhoneChange) Execute(ctx context.Context, in ResendAuthInput) (*attempt.AuthAttempt, error) {
	op := phoneOperation{OperationID: in.OperationID, Binding: in.Binding, RequestHash: in.RequestHash, Origin: in.Origin}
	if err := op.validate(); err != nil {
		return nil, err
	}
	if in.Channel != verification.SMS && in.Channel != verification.WhatsApp {
		return nil, personalErrors.ErrVerificationInput
	}
	d := u.dependencies
	scoped, actor, err := resolvePhoneChangeScope(ctx, d, in.Binding)
	if err != nil {
		return nil, err
	}
	var current *attempt.AuthAttempt
	replay := false
	// Exact resend retries never reconcile or send again, even after their
	// provider outcome. A new operation is the only explicit resend request.
	err = d.Phone.Phone.UoW.Command(scoped, func(tx shared.Transaction) error {
		var e error
		current, e = phoneChangeActor(d.Phone, tx, actor, in.Binding, false)
		if e != nil {
			return e
		}
		_, e = phoneChangeReceipt(d.Phone, tx, actor, op, "phone_change_resend")
		if errors.Is(e, personalErrors.ErrReceiptNotFound) {
			return nil
		}
		replay = e == nil
		return e
	})
	if err != nil {
		return nil, err
	}
	if replay {
		return current, nil
	}
	observed, err := u.get.Execute(ctx, in.Binding)
	if err != nil {
		return nil, err
	}
	var token attempt.OperationToken
	reserved := false
	for retry := 0; retry < 3; retry++ {
		found := false
		err = u.prepared.PreparedCommand(scoped, func(tx shared.Transaction) error {
			a, e := phoneChangeActor(d.Phone, tx, actor, in.Binding, false)
			if e != nil {
				return e
			}
			if _, e = phoneChangeReceipt(d.Phone, tx, actor, op, "phone_change_resend"); e == nil {
				found = true
				return nil
			} else if !errors.Is(e, personalErrors.ErrReceiptNotFound) {
				return e
			}
			if e = reservePhoneBudget(d.Phone.Phone, tx, a, in.Origin, budget.Send, d.Phone.Phone.Now()); e != nil {
				if _, winner := phoneChangeReceipt(d.Phone, tx, actor, op, "phone_change_resend"); winner == nil {
					return errPhoneOperationReplay
				}
				return e
			}
			return nil
		}, func(tx shared.Transaction) error {
			var e error
			current, e = phoneChangeActor(d.Phone, tx, actor, in.Binding, true)
			if e != nil {
				return e
			}
			if _, e = phoneChangeReceipt(d.Phone, tx, actor, op, "phone_change_resend"); e == nil {
				if !found {
					return errPhoneOperationReplay
				}
				return nil
			} else if !errors.Is(e, personalErrors.ErrReceiptNotFound) {
				return e
			}
			if current.Revision != observed.Attempt.Revision {
				return personalErrors.ErrAuthAttemptConflict
			}
			now := d.Phone.Phone.Now()
			delay := d.Policy.ResendCooldown
			if delay < d.Policy.ReconcileAfter {
				delay = d.Policy.ReconcileAfter
			}
			if current.ProviderOperation != nil && now.Before(current.ProviderOperation.StartedAt.Add(delay)) {
				return personalErrors.ErrAuthAttemptCooldown
			}
			revision := current.Revision
			if current.State == attempt.Uncertain {
				if e = current.ReleaseUncertain(phoneToken(current), now, delay); e != nil {
					return e
				}
			}
			token, e = current.ReserveSend(in.OperationID, in.RequestHash, in.Channel, now)
			if e != nil {
				return e
			}
			if e = d.Phone.Phone.Attempts.Save(tx, in.Binding, current, revision); e != nil {
				return e
			}
			r, e := newPhoneChangeReceipt(actor, op, "phone_change_resend", now)
			if e != nil {
				return e
			}
			if e = d.Phone.Phone.Receipts.Create(tx, r); e != nil {
				return e
			}
			reserved = true
			return nil
		})
		if !errors.Is(err, errPhoneOperationReplay) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if !reserved {
		return current, nil
	}
	result, providerErr := d.Phone.Phone.Provider.Start(ctx, verification.StartRequest{DestinationE164: current.DestinationE164, Channel: current.Channel})
	if providerErr != nil {
		return current, nil
	}
	return persistPhoneChangeOutcome(scoped, d.Phone, actor, in.Binding, token, result)
}
