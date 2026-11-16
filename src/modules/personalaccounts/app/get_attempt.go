package app

import (
	"context"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type GetAttemptDependencies struct {
	UoW             shared.UnitOfWork
	Attempts        repository.AuthAttemptRepository
	Provider        verification.VerificationProvider
	Now             func() time.Time
	ProviderTimeout time.Duration
	ReconcileAfter  time.Duration
}
type GetAttempt struct{ dependencies GetAttemptDependencies }

func NewGetAttempt(d GetAttemptDependencies) (*GetAttempt, error) {
	if d.UoW == nil || d.Attempts == nil || d.Provider == nil || d.Now == nil || d.ProviderTimeout <= 0 || d.ReconcileAfter < d.ProviderTimeout {
		return nil, personalErrors.ErrVerificationConfiguration
	}
	return &GetAttempt{dependencies: d}, nil
}

// Execute never calls Start or Check. Fetch approval is deliberately ignored:
// another attempt for the same phone may have approved a reused provider SID.
func (u *GetAttempt) Execute(ctx context.Context, binding attempt.Binding) (*attempt.AuthAttempt, error) {
	if binding.Validate() != nil {
		return nil, personalErrors.ErrInvalidAuthAttempt
	}
	if shared.PersonalAccountID(ctx) != uuid.Nil {
		return nil, shared.ErrPersonalScope
	}
	d := u.dependencies
	base := StartAuthDependencies{UoW: d.UoW, Attempts: d.Attempts, Provider: d.Provider, Now: d.Now}
	ctx = shared.WithPersonalAttempt(ctx, binding.ID, binding.ContinuationHash)
	a, err := readPhoneAttempt(ctx, base, binding)
	if err != nil {
		return nil, err
	}
	now := d.Now()
	if !now.Before(a.ExpiresAt) && (a.State == attempt.Pending || a.State == attempt.Uncertain || a.State == attempt.Verified) {
		return persistPhoneOutcome(ctx, base, binding, phoneToken(a), verification.Result{})
	}
	if a.State != attempt.Uncertain || a.ProviderReference == "" || a.ProviderOperation == nil || now.Before(a.ProviderOperation.StartedAt.Add(d.ReconcileAfter)) {
		return a, nil
	}
	token := phoneToken(a)
	result, providerErr := d.Provider.Fetch(ctx, verification.Binding{Reference: a.ProviderReference, DestinationE164: a.DestinationE164})
	if providerErr != nil || result.Reference != a.ProviderReference || result.State == verification.Approved {
		return readPhoneAttempt(ctx, base, binding)
	}
	return persistPhoneOutcome(ctx, base, binding, token, result)
}
