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

// CreateOperatorInput is UC-006 (POST /api/v1/users). Owner-only — the
// handler enforces that via middleware.
type CreateOperatorInput struct {
	GymID    uuid.UUID
	OwnerID  uuid.UUID
	FullName string
	Email    string
	Password string // optional: empty -> generate one
}

type CreateOperatorOutput struct {
	UserID   uuid.UUID
	Email    string
	Password string // plaintext, returned ONCE in the response
}

type CreateOperator struct {
	Users   userRepo.UserRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
	NowFunc func() time.Time
}

func NewCreateOperator(users userRepo.UserRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CreateOperator {
	return &CreateOperator{Users: users, UoW: uow, Audit: recorder, NowFunc: func() time.Time { return time.Now().UTC() }}
}

func (uc *CreateOperator) Execute(ctx context.Context, in CreateOperatorInput) (CreateOperatorOutput, error) {
	if err := userDomain.ValidateFullName(in.FullName); err != nil {
		return CreateOperatorOutput{}, sharedDomain.NewValidationError(err)
	}
	if !userDomain.ValidateEmail(in.Email) {
		return CreateOperatorOutput{}, sharedDomain.NewValidationError(userErrors.ErrInvalidEmail)
	}
	password := in.Password
	if password == "" {
		var err error
		password, err = auth.GenerateTempPassword()
		if err != nil {
			return CreateOperatorOutput{}, sharedDomain.NewUnexpectedError(err)
		}
	} else if err := userDomain.ValidatePassword(password); err != nil {
		return CreateOperatorOutput{}, sharedDomain.NewValidationError(err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return CreateOperatorOutput{}, sharedDomain.NewUnexpectedError(err)
	}

	now := uc.NowFunc()
	userID := uuid.New()
	owner := in.OwnerID
	user := userDomain.NewUser(userID, in.GymID, in.Email, hash, in.FullName, userDomain.RoleOperator, true, &owner, now)

	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		exists, err := uc.Users.ExistsByEmail(tx, in.Email)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if exists {
			return sharedDomain.NewBusinessError(userErrors.ErrEmailAlreadyExists, "")
		}
		count, err := uc.Users.CountOperatorsByGym(tx, in.GymID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if count >= userDomain.MaxOperatorsPerGym-1 {
			return sharedDomain.NewBusinessError(userErrors.ErrOperatorLimitReached, "")
		}
		if _, err := uc.Users.Create(tx, user); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "users",
			EntityID:    user.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.OwnerID,
			Changes:     map[string]any{"role": "operator", "email": user.Email},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
	if err != nil {
		return CreateOperatorOutput{}, err
	}
	return CreateOperatorOutput{UserID: user.ID, Email: user.Email, Password: password}, nil
}

// UpdateOperatorInput is UC-007 (PATCH /api/v1/users/{id}). Only the editable
// surface — role/active/password live in their own use cases.
type UpdateOperatorInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	TargetID    uuid.UUID
	FullName    *string
	Email       *string
	Phone       *string
}

type UpdateOperator struct {
	Users userRepo.UserRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewUpdateOperator(users userRepo.UserRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdateOperator {
	return &UpdateOperator{Users: users, UoW: uow, Audit: recorder}
}

func (uc *UpdateOperator) Execute(ctx context.Context, in UpdateOperatorInput) (*userDomain.User, error) {
	now := time.Now().UTC()
	var out *userDomain.User
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		u, err := uc.Users.GetByID(tx, in.TargetID)
		if err != nil {
			return err
		}
		if u.GymID != in.GymID {
			return sharedDomain.NewBusinessError(userErrors.ErrCrossGym, "")
		}
		if in.Email != nil && userDomain.ValidateEmail(*in.Email) && u.Email != *in.Email {
			exists, err := uc.Users.ExistsByEmail(tx, *in.Email)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if exists {
				return sharedDomain.NewBusinessError(userErrors.ErrEmailAlreadyExists, "")
			}
		}
		if err := u.UpdateProfile(in.FullName, in.Email, in.Phone, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		updated, err := uc.Users.Update(tx, u)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "users",
			EntityID:    in.TargetID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"full_name": in.FullName, "email": in.Email, "phone": in.Phone},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = updated
		return nil
	})
	return out, err
}

// ToggleOperatorActiveInput is UC-008 (PATCH /api/v1/users/{id}/active).
type ToggleOperatorActiveInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	TargetID    uuid.UUID
	Active      bool
}

type ToggleOperatorActive struct {
	Users     userRepo.UserRepository
	Blacklist userRepo.RefreshTokenBlacklist
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
}

func NewToggleOperatorActive(users userRepo.UserRepository, bl userRepo.RefreshTokenBlacklist,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *ToggleOperatorActive {
	return &ToggleOperatorActive{Users: users, Blacklist: bl, UoW: uow, Audit: recorder}
}

func (uc *ToggleOperatorActive) Execute(ctx context.Context, in ToggleOperatorActiveInput) (*userDomain.User, error) {
	now := time.Now().UTC()
	var out *userDomain.User
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		u, err := uc.Users.GetByID(tx, in.TargetID)
		if err != nil {
			return err
		}
		if u.GymID != in.GymID {
			return sharedDomain.NewBusinessError(userErrors.ErrCrossGym, "")
		}
		if err := u.SetActive(in.Active, in.ActorUserID, now); err != nil {
			return sharedDomain.NewBusinessError(err, "")
		}
		updated, err := uc.Users.Update(tx, u)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if !in.Active && uc.Blacklist != nil {
			if err := uc.Blacklist.RevokeAllForUser(tx, u.ID, now.Add(auth.RefreshTokenDuration)); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "users",
			EntityID:    in.TargetID,
			Action:      audit.ActionToggleActive,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"active": in.Active},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = updated
		return nil
	})
	return out, err
}

// ResetOperatorPasswordInput is UC-009 (POST /api/v1/users/{id}/reset-password).
type ResetOperatorPasswordInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	TargetID    uuid.UUID
}

type ResetOperatorPasswordOutput struct {
	UserID   uuid.UUID
	Password string
}

type ResetOperatorPassword struct {
	Users     userRepo.UserRepository
	Blacklist userRepo.RefreshTokenBlacklist
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
}

func NewResetOperatorPassword(users userRepo.UserRepository, bl userRepo.RefreshTokenBlacklist,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *ResetOperatorPassword {
	return &ResetOperatorPassword{Users: users, Blacklist: bl, UoW: uow, Audit: recorder}
}

func (uc *ResetOperatorPassword) Execute(ctx context.Context, in ResetOperatorPasswordInput) (ResetOperatorPasswordOutput, error) {
	password, err := auth.GenerateTempPassword()
	if err != nil {
		return ResetOperatorPasswordOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return ResetOperatorPasswordOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	now := time.Now().UTC()
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		u, err := uc.Users.GetByID(tx, in.TargetID)
		if err != nil {
			return err
		}
		if u.GymID != in.GymID {
			return sharedDomain.NewBusinessError(userErrors.ErrCrossGym, "")
		}
		u.ApplyPassword(hash, true, now)
		if _, err := uc.Users.Update(tx, u); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if uc.Blacklist != nil {
			if err := uc.Blacklist.RevokeAllForUser(tx, u.ID, now.Add(auth.RefreshTokenDuration)); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "users",
			EntityID:    in.TargetID,
			Action:      audit.ActionAdminPasswordReset,
			ActorUserID: &in.ActorUserID,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
	if err != nil {
		return ResetOperatorPasswordOutput{}, err
	}
	return ResetOperatorPasswordOutput{UserID: in.TargetID, Password: password}, nil
}
