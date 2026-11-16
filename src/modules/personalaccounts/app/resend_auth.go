package app

import (
	"context"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type PhoneRetryPolicy struct {
	ProviderTimeout time.Duration
	ReconcileAfter  time.Duration
	ResendCooldown  time.Duration
}
type ResendAuth struct {
	dependencies StartAuthDependencies
	get          *GetAttempt
	minDelay     time.Duration
}
type ResendAuthInput struct {
	OperationID uuid.UUID
	Binding     attempt.Binding
	RequestHash [32]byte
	Channel     verification.Channel
	Origin      string
}

func NewResendAuth(d StartAuthDependencies, policy PhoneRetryPolicy) (*ResendAuth, error) {
	if err := validatePhoneDependencies(d); err != nil {
		return nil, err
	}
	if policy.ResendCooldown <= 0 {
		return nil, personalErrors.ErrVerificationConfiguration
	}
	get, err := NewGetAttempt(GetAttemptDependencies{UoW: d.UoW, Attempts: d.Attempts, Provider: d.Provider, Now: d.Now, ProviderTimeout: policy.ProviderTimeout, ReconcileAfter: policy.ReconcileAfter})
	if err != nil {
		return nil, err
	}
	delay := policy.ResendCooldown
	if delay < policy.ReconcileAfter {
		delay = policy.ReconcileAfter
	}
	return &ResendAuth{dependencies: d, get: get, minDelay: delay}, nil
}

// Execute is a new, explicit action with its own operation UUID. Receipt
// replay, polling and provider ambiguity never trigger another send.
func (u *ResendAuth) Execute(ctx context.Context, in ResendAuthInput) (*attempt.AuthAttempt, error) {
	d := u.dependencies
	op := phoneOperation{OperationID: in.OperationID, Binding: in.Binding, RequestHash: in.RequestHash, Origin: in.Origin}
	if err := op.validate(); err != nil {
		return nil, err
	}
	if in.Channel != verification.SMS && in.Channel != verification.WhatsApp {
		return nil, personalErrors.ErrVerificationInput
	}
	if shared.PersonalAccountID(ctx) != uuid.Nil {
		return nil, shared.ErrPersonalScope
	}
	ctx = shared.WithPersonalAttempt(ctx, in.Binding.ID, in.Binding.ContinuationHash)
	observed, replay, err := readPhoneOperation(ctx, d, op, "auth_resend")
	if err != nil {
		return nil, err
	}
	if replay {
		return observed, nil
	}
	// If the previous call has a known SID, attempt read-only reconciliation
	// before releasing uncertainty. Fetch approved still cannot authorize OTP.
	observed, err = u.get.Execute(ctx, in.Binding)
	if err != nil {
		return nil, err
	}
	var current *attempt.AuthAttempt
	var token attempt.OperationToken
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if replay, err := phoneReceipt(d, tx, op, "auth_resend"); err != nil {
			return err
		} else if replay {
			return errPhoneOperationReplay
		}
		before, err := d.Attempts.Get(tx, in.Binding)
		if err != nil {
			return err
		}
		if err = phoneAttemptScope(before); err != nil {
			return err
		}
		now := d.Now()
		if err = reservePhoneBudget(d, tx, before, in.Origin, budget.Send, now); err != nil {
			return err
		}
		current, err = d.Attempts.GetForUpdate(tx, in.Binding)
		if err != nil {
			return err
		}
		if replay, err := phoneReceipt(d, tx, op, "auth_resend"); err != nil {
			return err
		} else if replay {
			return errPhoneOperationReplay
		}
		if err = phoneAttemptScope(current); err != nil {
			return err
		}
		if current.Revision != observed.Revision {
			return personalErrors.ErrAuthAttemptConflict
		}
		now = d.Now()
		revision := current.Revision
		// 060 stores the latest operation, not last_send_at. Waiting since a
		// newer check is conservative and cannot shorten the send cooldown.
		if current.ProviderOperation != nil && now.Before(current.ProviderOperation.StartedAt.Add(u.minDelay)) {
			return personalErrors.ErrAuthAttemptCooldown
		}
		if current.State == attempt.Uncertain {
			if err = current.ReleaseUncertain(phoneToken(current), now, u.minDelay); err != nil {
				return err
			}
		}
		token, err = current.ReserveSend(in.OperationID, in.RequestHash, in.Channel, now)
		if err != nil {
			return err
		}
		if err = d.Attempts.Save(tx, in.Binding, current, revision); err != nil {
			return err
		}
		return createPhoneReceipt(d, tx, op, "auth_resend", now)
	})
	if errors.Is(err, errPhoneOperationReplay) {
		current, _, err = readPhoneOperation(ctx, d, op, "auth_resend")
		return current, err
	}
	if err != nil {
		return nil, err
	}
	result, providerErr := d.Provider.Start(ctx, verification.StartRequest{DestinationE164: current.DestinationE164, Channel: current.Channel})
	if providerErr != nil {
		return current, nil
	}
	return persistPhoneOutcome(ctx, d, in.Binding, token, result)
}
