package app

import (
	"context"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

type AuthStatusReader interface {
	Execute(context.Context, attempt.Binding) (AccessResult, error)
}
type AuthAttemptResender interface {
	Execute(context.Context, ResendAuthInput) (*attempt.AuthAttempt, error)
}
type GetAuthStatusRouter struct {
	uow      shared.UnitOfWork
	attempts repository.AuthAttemptRepository
	access   AuthStatusReader
	phone    *GetPhoneChangeAttempt
}
type ResendAuthRouter struct {
	uow      shared.UnitOfWork
	attempts repository.AuthAttemptRepository
	access   AuthAttemptResender
	phone    *ResendPhoneChange
}

func NewGetAuthStatusRouter(uow shared.UnitOfWork, attempts repository.AuthAttemptRepository, access AuthStatusReader, phone *GetPhoneChangeAttempt) (*GetAuthStatusRouter, error) {
	if uow == nil || attempts == nil || access == nil || phone == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	if _, ok := uow.(shared.PersonalAuthPreparedUnitOfWork); !ok {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &GetAuthStatusRouter{uow, attempts, access, phone}, nil
}
func NewResendAuthRouter(uow shared.UnitOfWork, attempts repository.AuthAttemptRepository, access AuthAttemptResender, phone *ResendPhoneChange) (*ResendAuthRouter, error) {
	if uow == nil || attempts == nil || access == nil || phone == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	if _, ok := uow.(shared.PersonalAuthPreparedUnitOfWork); !ok {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &ResendAuthRouter{uow, attempts, access, phone}, nil
}
func peekAuthAttempt(ctx context.Context, uow shared.UnitOfWork, repo repository.AuthAttemptRepository, b attempt.Binding) (attempt.Purpose, error) {
	if err := shared.ValidatePersonalAnonymousScope(ctx); err != nil {
		return "", err
	}
	if b.Validate() != nil {
		return "", personalErrors.ErrInvalidAuthAttempt
	}
	q, err := uow.Query(shared.WithPersonalAttempt(ctx, b.ID, b.ContinuationHash))
	if err != nil {
		return "", err
	}
	defer shared.CloseQuery(q)
	a, err := repo.Get(q, b)
	if err != nil {
		return "", err
	}
	switch a.Purpose {
	case attempt.Access, attempt.Recovery, attempt.PhoneChange:
		return a.Purpose, nil
	default:
		return "", personalErrors.ErrAuthAttemptState
	}
}
func (u *GetAuthStatusRouter) Execute(ctx context.Context, b attempt.Binding) (AccessResult, error) {
	purpose, err := peekAuthAttempt(ctx, u.uow, u.attempts, b)
	if err != nil {
		return AccessResult{}, err
	}
	if purpose == attempt.PhoneChange {
		return u.phone.Execute(ctx, b)
	}
	return u.access.Execute(ctx, b)
}
func (u *ResendAuthRouter) Execute(ctx context.Context, in ResendAuthInput) (*attempt.AuthAttempt, error) {
	purpose, err := peekAuthAttempt(ctx, u.uow, u.attempts, in.Binding)
	if err != nil {
		return nil, err
	}
	if purpose == attempt.PhoneChange {
		return u.phone.Execute(ctx, in)
	}
	return u.access.Execute(ctx, in)
}
