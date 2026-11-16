package app

import (
	"context"
	"errors"
	"math"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type CheckPhoneVerification struct {
	dependencies StartAuthDependencies
	maxChecks    int
}
type CheckPhoneVerificationInput struct {
	OperationID uuid.UUID
	Binding     attempt.Binding
	RequestHash [32]byte
	Code        string
	Origin      string
}

func NewCheckPhoneVerification(d StartAuthDependencies, maxChecks int) (*CheckPhoneVerification, error) {
	if err := validatePhoneDependencies(d); err != nil {
		return nil, err
	}
	if maxChecks <= 0 || maxChecks > math.MaxInt32 {
		return nil, personalErrors.ErrVerificationConfiguration
	}
	return &CheckPhoneVerification{dependencies: d, maxChecks: maxChecks}, nil
}

// Execute proves only phone possession. Device signatures, recovery keys,
// identity resolution, sessions and all finalization remain separate commands.
func (u *CheckPhoneVerification) Execute(ctx context.Context, in CheckPhoneVerificationInput) (*attempt.AuthAttempt, error) {
	d := u.dependencies
	op := phoneOperation{OperationID: in.OperationID, Binding: in.Binding, RequestHash: in.RequestHash, Origin: in.Origin}
	if err := op.validate(); err != nil {
		return nil, err
	}
	if len(in.Code) < 4 || len(in.Code) > 10 {
		return nil, personalErrors.ErrVerificationInput
	}
	for _, digit := range in.Code {
		if digit < '0' || digit > '9' {
			return nil, personalErrors.ErrVerificationInput
		}
	}
	if shared.PersonalAccountID(ctx) != uuid.Nil {
		return nil, shared.ErrPersonalScope
	}
	ctx = shared.WithPersonalAttempt(ctx, in.Binding.ID, in.Binding.ContinuationHash)
	observed, replay, err := readPhoneOperation(ctx, d, op, "auth_check")
	if err != nil {
		return nil, err
	}
	if replay {
		return observed, nil
	}
	var token attempt.OperationToken
	var current *attempt.AuthAttempt
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if replay, err := phoneReceipt(d, tx, op, "auth_check"); err != nil {
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
		if err = reservePhoneBudget(d, tx, before, in.Origin, budget.Check, now); err != nil {
			return err
		}
		current, err = d.Attempts.GetForUpdate(tx, in.Binding)
		if err != nil {
			return err
		}
		if replay, err := phoneReceipt(d, tx, op, "auth_check"); err != nil {
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
		token, err = current.ReserveCheck(in.OperationID, in.RequestHash, now, u.maxChecks)
		if err != nil {
			return err
		}
		if err = d.Attempts.Save(tx, in.Binding, current, revision); err != nil {
			return err
		}
		return createPhoneReceipt(d, tx, op, "auth_check", now)
	})
	if errors.Is(err, errPhoneOperationReplay) {
		current, _, err = readPhoneOperation(ctx, d, op, "auth_check")
		return current, err
	}
	if err != nil {
		return nil, err
	}
	result, providerErr := d.Provider.Check(ctx, verification.CheckRequest{Binding: verification.Binding{Reference: current.ProviderReference, DestinationE164: current.DestinationE164}, Code: in.Code})
	if providerErr != nil {
		return current, nil
	}
	return persistPhoneOutcome(ctx, d, in.Binding, token, result)
}
