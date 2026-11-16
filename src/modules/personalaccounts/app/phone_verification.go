package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// This signal must escape Command: committing here would charge the budget a
// second time for an idempotent operation discovered after waiting for locks.
var errPhoneOperationReplay = errors.New("phone operation already reserved")

type phoneOperation struct {
	OperationID uuid.UUID
	Binding     attempt.Binding
	RequestHash [32]byte
	Origin      string
}

func validatePhoneDependencies(d StartAuthDependencies) error {
	if _, err := NewStartAuth(d); err != nil {
		return err
	}
	_, err := d.Rules.Plan(time.Unix(1, 0), budget.Send, map[budget.Scope][32]byte{budget.Destination: {1}, budget.Origin: {2}, budget.Service: {3}})
	return err
}
func (in phoneOperation) validate() error {
	if in.OperationID == uuid.Nil || in.Binding.Validate() != nil || in.RequestHash == [32]byte{} || strings.TrimSpace(in.Origin) == "" || len(in.Origin) > 512 {
		return personalErrors.ErrInvalidAuthAttempt
	}
	return nil
}
func phoneAttemptScope(a *attempt.AuthAttempt) error {
	if a == nil || (a.Purpose != attempt.Access && a.Purpose != attempt.Recovery) {
		return personalErrors.ErrAuthAttemptState
	}
	return nil
}
func phoneReceipt(d StartAuthDependencies, tx shared.Transaction, in phoneOperation, purpose string) (bool, error) {
	r, err := d.Receipts.Get(tx, receipt.Owner{AttemptID: in.Binding.ID}, in.OperationID)
	if errors.Is(err, personalErrors.ErrReceiptNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = r.Match(purpose, in.RequestHash); err != nil {
		return false, err
	}
	return true, nil
}
func readPhoneOperation(ctx context.Context, d StartAuthDependencies, in phoneOperation, purpose string) (*attempt.AuthAttempt, bool, error) {
	tx, err := d.UoW.Query(ctx)
	if err != nil {
		return nil, false, err
	}
	defer shared.CloseQuery(tx)
	a, err := d.Attempts.Get(tx, in.Binding)
	if err != nil {
		return nil, false, err
	}
	if err = phoneAttemptScope(a); err != nil {
		return nil, false, err
	}
	found, err := phoneReceipt(d, tx, in, purpose)
	return a, found, err
}
func readPhoneAttempt(ctx context.Context, d StartAuthDependencies, binding attempt.Binding) (*attempt.AuthAttempt, error) {
	tx, err := d.UoW.Query(ctx)
	if err != nil {
		return nil, err
	}
	defer shared.CloseQuery(tx)
	a, err := d.Attempts.Get(tx, binding)
	if err != nil {
		return nil, err
	}
	if err = phoneAttemptScope(a); err != nil {
		return nil, err
	}
	return a, nil
}
func reservePhoneBudget(d StartAuthDependencies, tx shared.Transaction, a *attempt.AuthAttempt, origin string, action budget.Action, now time.Time) error {
	subjects := map[budget.Scope][32]byte{}
	for scope, value := range map[budget.Scope]string{budget.Destination: a.DestinationE164, budget.Origin: origin, budget.Service: d.ServiceIdentity} {
		hash, err := d.Subjects.Hash(scope, value)
		if err != nil {
			return err
		}
		subjects[scope] = hash
	}
	plan, err := d.Rules.Plan(now, action, subjects)
	if err != nil {
		return err
	}
	return d.Budgets.Reserve(tx, plan)
}
func createPhoneReceipt(d StartAuthDependencies, tx shared.Transaction, in phoneOperation, purpose string, now time.Time) error {
	r, err := receipt.New(in.OperationID, receipt.Owner{AttemptID: in.Binding.ID}, purpose, in.RequestHash, now)
	if err != nil {
		return err
	}
	r.ResultReference = in.Binding.ID
	return d.Receipts.Create(tx, r)
}
func phoneToken(a *attempt.AuthAttempt) attempt.OperationToken {
	if a.ProviderOperation == nil {
		return attempt.OperationToken{}
	}
	return attempt.OperationToken{ID: a.ProviderOperation.ID, RequestHash: a.ProviderOperation.RequestHash, Kind: a.ProviderOperation.Kind, Revision: a.Revision}
}

// persistPhoneOutcome never calls the provider. A stale token returns the
// current state; it cannot repeat a call or overwrite a later explicit action.
func persistPhoneOutcome(ctx context.Context, d StartAuthDependencies, binding attempt.Binding, token attempt.OperationToken, result verification.Result) (*attempt.AuthAttempt, error) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var current *attempt.AuthAttempt
	err := d.UoW.Command(persistCtx, func(tx shared.Transaction) error {
		stored, err := d.Attempts.GetForUpdate(tx, binding)
		if err != nil {
			return err
		}
		if err = phoneAttemptScope(stored); err != nil {
			return err
		}
		current = stored
		now := d.Now()
		revision := stored.Revision
		if !now.Before(stored.ExpiresAt) && (stored.State == attempt.Pending || stored.State == attempt.Uncertain || stored.State == attempt.Verified) {
			if err = stored.Expire(now); err != nil {
				return err
			}
			return d.Attempts.Save(tx, binding, stored, revision)
		}
		if err = stored.ApplyOutcome(token, result, now); err != nil {
			if errors.Is(err, personalErrors.ErrAuthAttemptOperation) || errors.Is(err, personalErrors.ErrAuthAttemptState) || errors.Is(err, personalErrors.ErrAuthAttemptExpired) {
				return nil
			}
			return err
		}
		return d.Attempts.Save(tx, binding, stored, revision)
	})
	if err != nil {
		return nil, err
	}
	return current, nil
}
