package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// AssignSelfPINInput is the payload for "the authenticated user sets / changes
// their own reception PIN" — UC-013 in the auth-refactor plan. Owners use it
// for the kiosk-style desktop login; operators get the same surface once the
// multi-operator UI lights up in fase 2 (the use case is already neutral).
type AssignSelfPINInput struct {
	GymID  uuid.UUID
	UserID uuid.UUID
	PIN    string
}

type AssignSelfPIN struct {
	Users   userRepo.UserRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
	NowFunc func() time.Time
}

func NewAssignSelfPIN(users userRepo.UserRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *AssignSelfPIN {
	return &AssignSelfPIN{Users: users, UoW: uow, Audit: recorder, NowFunc: func() time.Time { return time.Now().UTC() }}
}

func (uc *AssignSelfPIN) Execute(ctx context.Context, in AssignSelfPINInput) (*userDomain.User, error) {
	if err := userDomain.ValidatePIN(in.PIN); err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	hash, err := auth.HashPIN(in.PIN)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := uc.NowFunc()
	var out *userDomain.User
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		u, err := uc.Users.GetByID(tx, in.UserID)
		if err != nil {
			return err
		}
		if u.GymID != in.GymID {
			return sharedDomain.NewBusinessError(userErrors.ErrCrossGym, "")
		}
		if !u.Active {
			return sharedDomain.NewBusinessError(userErrors.ErrAccountInactive, "")
		}
		u.AssignPIN(hash, now)
		updated, err := uc.Users.Update(tx, u)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "users",
			EntityID:    in.UserID,
			Action:      audit.ActionAssignPIN,
			ActorUserID: &in.UserID,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = updated
		return nil
	})
	return out, err
}

// ClearSelfPIN removes the authenticated user's PIN. Used when the operator
// forgets it (the recovery loop is: log in with email+password, clear, set a
// new one). The handler exposes this as DELETE /api/v1/auth/me/pin.
type ClearSelfPINInput struct {
	GymID  uuid.UUID
	UserID uuid.UUID
}

type ClearSelfPIN struct {
	Users   userRepo.UserRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
	NowFunc func() time.Time
}

func NewClearSelfPIN(users userRepo.UserRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *ClearSelfPIN {
	return &ClearSelfPIN{Users: users, UoW: uow, Audit: recorder, NowFunc: func() time.Time { return time.Now().UTC() }}
}

func (uc *ClearSelfPIN) Execute(ctx context.Context, in ClearSelfPINInput) error {
	now := uc.NowFunc()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		u, err := uc.Users.GetByID(tx, in.UserID)
		if err != nil {
			return err
		}
		if u.GymID != in.GymID {
			return sharedDomain.NewBusinessError(userErrors.ErrCrossGym, "")
		}
		if !u.HasPIN() {
			// Idempotent — the caller's request is satisfied either way.
			return nil
		}
		u.ClearPIN(now)
		if _, err := uc.Users.Update(tx, u); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "users",
			EntityID:    in.UserID,
			Action:      audit.ActionClearPIN,
			ActorUserID: &in.UserID,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
}
