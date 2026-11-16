package app

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type StartAuthDependencies struct {
	UoW              shared.UnitOfWork
	Attempts         repository.AuthAttemptRepository
	Receipts         repository.AuthReceiptRepository
	Budgets          budget.Repository
	Rules            budget.Rules
	Subjects         budget.SubjectHasher
	Provider         verification.VerificationProvider
	ServiceIdentity  string
	ProviderValidity time.Duration
	Now              func() time.Time
}
type StartAuth struct{ dependencies StartAuthDependencies }
type StartAuthInput struct {
	OperationID       uuid.UUID
	Binding           attempt.Binding
	RequestHash       [32]byte
	ProposedAccountID uuid.UUID
	Candidate         attempt.Candidate
	Purpose           attempt.Purpose
	DestinationE164   string
	Channel           verification.Channel
	// Trusted canonical origin from the HTTP edge, never a body/header value
	// supplied by the caller without a trusted-proxy policy.
	Origin string
}

func NewStartAuth(d StartAuthDependencies) (*StartAuth, error) {
	if d.UoW == nil || d.Attempts == nil || d.Receipts == nil || d.Budgets == nil || d.Subjects == nil || d.Provider == nil || d.Now == nil || d.ServiceIdentity == "" || d.ProviderValidity < 2*time.Minute || d.ProviderValidity > 24*time.Hour {
		return nil, personalErrors.ErrVerificationConfiguration
	}
	return &StartAuth{dependencies: d}, nil
}

// Execute reserves the shared capacity and the provider intent durably, then
// leaves the transaction for exactly one send. Neither a retry, a process crash
// nor an ambiguous provider outcome automatically sends another code.
func (u *StartAuth) Execute(ctx context.Context, in StartAuthInput) (*attempt.AuthAttempt, error) {
	d := u.dependencies
	if (in.Purpose != attempt.Access && in.Purpose != attempt.Recovery) || in.OperationID == uuid.Nil || in.Origin == "" || len(in.Origin) > 512 {
		return nil, personalErrors.ErrInvalidAuthAttempt
	}
	if shared.PersonalAccountID(ctx) != uuid.Nil {
		return nil, shared.ErrPersonalScope
	}
	ctx = shared.WithPersonalAttempt(ctx, in.Binding.ID, in.Binding.ContinuationHash)
	var current *attempt.AuthAttempt
	var token attempt.OperationToken
	reserved := false
	err := d.UoW.Command(ctx, func(tx shared.Transaction) error {
		// Reads before budget locks do not lock an existing challenge. The new
		// attempt path always obtains shared budget locks before inserting it.
		existing, err := d.Attempts.Get(tx, in.Binding)
		if err == nil {
			if existing.RequestHash != in.RequestHash {
				return personalErrors.ErrIdempotencyMismatch
			}
			current = existing
			return nil
		}
		if !errors.Is(err, personalErrors.ErrAuthAttemptNotFound) {
			return err
		}
		now := d.Now()
		var nonce [32]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return personalErrors.ErrVerificationUnavailable
		}
		current, err = attempt.New(attempt.NewAttempt{Binding: in.Binding, ProposedAccountID: in.ProposedAccountID, Candidate: in.Candidate, Nonce: nonce, Purpose: in.Purpose, DestinationE164: in.DestinationE164, Channel: in.Channel, RequestHash: in.RequestHash}, now, d.ProviderValidity)
		if err != nil {
			return err
		}
		subjects := map[budget.Scope][32]byte{}
		for scope, value := range map[budget.Scope]string{budget.Destination: in.DestinationE164, budget.Origin: in.Origin, budget.Service: d.ServiceIdentity} {
			hash, err := d.Subjects.Hash(scope, value)
			if err != nil {
				return err
			}
			subjects[scope] = hash
		}
		plan, err := d.Rules.Plan(now, budget.Send, subjects)
		if err != nil {
			return err
		}
		if err = d.Budgets.Reserve(tx, plan); err != nil {
			return err
		}
		if err = d.Attempts.Create(tx, current); err != nil {
			return err
		}
		revision := current.Revision
		token, err = current.ReserveSend(in.OperationID, in.RequestHash, in.Channel, now)
		if err != nil {
			return err
		}
		if err = d.Attempts.Save(tx, in.Binding, current, revision); err != nil {
			return err
		}
		r, err := receipt.New(in.OperationID, receipt.Owner{AttemptID: in.Binding.ID}, "auth_start", in.RequestHash, now)
		if err != nil {
			return err
		}
		r.ResultReference = in.Binding.ID
		if err = d.Receipts.Create(tx, r); err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if err != nil {
		// Another instance may have inserted this same attempt after our first
		// read. Its transaction owns the send; read its state without reserving.
		if errors.Is(err, personalErrors.ErrAuthAttemptConflict) {
			q, readErr := d.UoW.Query(ctx)
			if readErr != nil {
				return nil, readErr
			}
			defer shared.CloseQuery(q)
			winner, readErr := d.Attempts.Get(q, in.Binding)
			if readErr != nil {
				return nil, readErr
			}
			if winner.RequestHash != in.RequestHash {
				return nil, personalErrors.ErrIdempotencyMismatch
			}
			return winner, nil
		}
		return nil, err
	}
	if !reserved {
		return current, nil
	}
	result, providerErr := d.Provider.Start(ctx, verification.StartRequest{DestinationE164: current.DestinationE164, Channel: current.Channel})
	if providerErr != nil {
		return current, nil
	} // Uncertain intent is already durable.
	// A disconnected client must not prevent recording a provider response we
	// did receive. This bounded, detached transaction never repeats the call.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err = d.UoW.Command(persistCtx, func(tx shared.Transaction) error {
		stored, err := d.Attempts.GetForUpdate(tx, in.Binding)
		if err != nil {
			return err
		}
		revision := stored.Revision
		if err = stored.ApplyOutcome(token, result, d.Now()); err != nil {
			return err
		}
		if err = d.Attempts.Save(tx, in.Binding, stored, revision); err != nil {
			return err
		}
		current = stored
		return nil
	})
	if err != nil {
		return nil, err
	}
	return current, nil
}
