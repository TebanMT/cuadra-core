package app

import (
	"context"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
)

type PhoneChecker interface {
	Execute(context.Context, CheckPhoneVerificationInput) (*attempt.AuthAttempt, error)
}
type AccessFinalizer interface {
	Execute(context.Context, FinalizeAccessInput) (AccessResult, error)
}
type AttemptReader interface {
	Execute(context.Context, attempt.Binding) (*attempt.AuthAttempt, error)
}

type CheckAccessInput struct {
	CheckPhoneVerificationInput
	DeviceSignature string
}
type CheckAccess struct {
	check    PhoneChecker
	finalize AccessFinalizer
}

func NewCheckAccess(check PhoneChecker, finalize AccessFinalizer) (*CheckAccess, error) {
	if check == nil || finalize == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &CheckAccess{check: check, finalize: finalize}, nil
}

// Phone possession and account authorization are separate commits. Retrying
// this command after either commit never repeats the provider check.
func (u *CheckAccess) Execute(ctx context.Context, in CheckAccessInput) (AccessResult, error) {
	a, err := u.check.Execute(ctx, in.CheckPhoneVerificationInput)
	if err != nil {
		return AccessResult{}, err
	}
	if a.Purpose == attempt.Access && (a.State == attempt.Verified || a.State == attempt.Consumed) {
		return u.finalize.Execute(ctx, FinalizeAccessInput{Binding: in.Binding, OperationID: in.OperationID, RequestHash: in.RequestHash, DeviceSignature: in.DeviceSignature})
	}
	return AccessResult{Attempt: a, AuthorizationRequired: a.State == attempt.Verified}, nil
}

type GetAuthStatus struct {
	read           AttemptReader
	finalize       AccessFinalizer
	authorizations DeviceAuthorizationStatusReader
	recovery       RecoveryStatusReader
}

type DeviceAuthorizationStatusReader interface {
	Execute(context.Context, attempt.Binding) (DeviceAuthorizationStatusResult, error)
}
type RecoveryStatusReader interface {
	Execute(context.Context, attempt.Binding) (AccessResult, error)
}

func NewGetAuthStatus(read AttemptReader, finalize AccessFinalizer, authorizations DeviceAuthorizationStatusReader, recovery RecoveryStatusReader) (*GetAuthStatus, error) {
	if read == nil || finalize == nil || authorizations == nil || recovery == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &GetAuthStatus{read: read, finalize: finalize, authorizations: authorizations, recovery: recovery}, nil
}

// A status query can retrieve an already committed result. It cannot create a
// session or use a provider's Fetch approval as proof of a submitted OTP.
func (u *GetAuthStatus) Execute(ctx context.Context, binding attempt.Binding) (AccessResult, error) {
	a, err := u.read.Execute(ctx, binding)
	if err != nil {
		return AccessResult{}, err
	}
	if a.State == attempt.Consumed {
		if a.Purpose == attempt.Recovery {
			return u.recovery.Execute(ctx, binding)
		}
		op := a.ProviderOperation
		if a.Purpose != attempt.Access || op == nil || op.Kind != attempt.Check || a.ResultReference == nil {
			return AccessResult{}, personalErrors.ErrAuthAttemptState
		}
		if *a.ResultReference != op.ID {
			return u.authorizationStatus(ctx, binding)
		}
		// The exact operation identity is persisted, never supplied by a query
		// parameter. FinalizeAccess's consumed branch checks the live session.
		return u.finalize.Execute(ctx, FinalizeAccessInput{Binding: binding, OperationID: op.ID, RequestHash: op.RequestHash})
	}
	if a.Purpose == attempt.Access && a.AccountID != nil {
		return u.authorizationStatus(ctx, binding)
	}
	return AccessResult{Attempt: a, AuthorizationRequired: a.State == attempt.Verified && (a.AccountID != nil || a.Purpose == attempt.Recovery)}, nil
}

func (u *GetAuthStatus) authorizationStatus(ctx context.Context, binding attempt.Binding) (AccessResult, error) {
	result, err := u.authorizations.Execute(ctx, binding)
	if err != nil {
		return AccessResult{}, err
	}
	if result.Attempt == nil {
		return AccessResult{}, personalErrors.ErrAuthAttemptState
	}
	return AccessResult{Attempt: result.Attempt, Authorized: result.Authorized, StateOverride: result.StateOverride, AuthorizationRequired: result.StateOverride == "" && result.Attempt.State == attempt.Verified}, nil
}
