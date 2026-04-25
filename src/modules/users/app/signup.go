// Package app — users + gyms use cases (UC-001 to UC-010).
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// SignupOwnerInput is the wizard step 1 payload (UC-001 step 1).
type SignupOwnerInput struct {
	FullName        string
	Email           string
	Password        string
	PasswordConfirm string
}

// SignupOwnerOutput mirrors the JSON the handler returns to the wizard.
type SignupOwnerOutput struct {
	UserID         uuid.UUID
	GymID          uuid.UUID
	AccessToken    string
	RefreshToken   string
	SetupCompleted bool
}

// SignupOwner is the UC-001 step 1 use case: register a new owner + a
// placeholder Gym in trial mode. Everything happens inside one
// UoW.Command — if anything fails (email collision, hash, audit), the gym
// row is rolled back too.
type SignupOwner struct {
	Users     userRepo.UserRepository
	Gyms      gymRepo.GymRepository
	UoW       sharedDomain.UnitOfWork
	Tokens    auth.TokenService
	Audit     audit.Recorder
	TrialDays int
	NowFunc   func() time.Time // injectable for tests
}

func NewSignupOwner(users userRepo.UserRepository, gyms gymRepo.GymRepository,
	uow sharedDomain.UnitOfWork, tokens auth.TokenService, recorder audit.Recorder, trialDays int) *SignupOwner {
	return &SignupOwner{
		Users:     users,
		Gyms:      gyms,
		UoW:       uow,
		Tokens:    tokens,
		Audit:     recorder,
		TrialDays: trialDays,
		NowFunc:   func() time.Time { return time.Now().UTC() },
	}
}

func (uc *SignupOwner) Execute(ctx context.Context, in SignupOwnerInput) (SignupOwnerOutput, error) {
	if err := userDomain.ValidateFullName(in.FullName); err != nil {
		return SignupOwnerOutput{}, sharedDomain.NewValidationError(err)
	}
	if !userDomain.ValidateEmail(in.Email) {
		return SignupOwnerOutput{}, sharedDomain.NewValidationError(userErrors.ErrInvalidEmail)
	}
	if err := userDomain.ValidatePassword(in.Password); err != nil {
		return SignupOwnerOutput{}, sharedDomain.NewValidationError(err)
	}
	if in.Password != in.PasswordConfirm {
		return SignupOwnerOutput{}, sharedDomain.NewValidationError(userErrors.ErrPasswordMismatch)
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return SignupOwnerOutput{}, sharedDomain.NewUnexpectedError(err)
	}

	now := uc.NowFunc()
	gymID := uuid.New()
	userID := uuid.New()

	gym := gymDomain.NewTrialGym(gymID, uc.TrialDays, now)
	user := userDomain.NewUser(userID, gymID, in.Email, hash, in.FullName, userDomain.RoleOwner, false, nil, now)

	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		exists, err := uc.Users.ExistsByEmail(tx, in.Email)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if exists {
			return sharedDomain.NewBusinessError(userErrors.ErrEmailAlreadyExists, "")
		}
		if _, err := uc.Gyms.Create(tx, gym); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if _, err := uc.Users.Create(tx, user); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       gymID,
			EntityType:  "users",
			EntityID:    userID,
			Action:      audit.ActionCreate,
			ActorUserID: &userID,
			Changes:     map[string]any{"role": "owner", "self_signup": true},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
	if err != nil {
		return SignupOwnerOutput{}, err
	}

	access, err := uc.Tokens.GenerateAccessToken(userID, gymID, userDomain.RoleOwner)
	if err != nil {
		return SignupOwnerOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	refresh, err := uc.Tokens.GenerateRefreshToken(userID, gymID, userDomain.RoleOwner)
	if err != nil {
		return SignupOwnerOutput{}, sharedDomain.NewUnexpectedError(err)
	}

	return SignupOwnerOutput{
		UserID:         userID,
		GymID:          gymID,
		AccessToken:    access,
		RefreshToken:   refresh,
		SetupCompleted: false,
	}, nil
}
