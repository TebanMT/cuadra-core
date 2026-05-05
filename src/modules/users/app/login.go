package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// LoginInput is UC-002. Email is normalised inside the use case; rate limiting
// is at the middleware/proxy layer (Caddy or fail2ban) — UC-002 DA-2.2 says
// 5/15min per IP; we leave that to infra and do NOT pretend to handle it here.
type LoginInput struct {
	Email    string
	Password string
}

type LoginOutput struct {
	UserID             uuid.UUID
	GymID              uuid.UUID
	FullName           string
	Email              string
	Role               string
	GymName            *string
	AccessToken        string
	RefreshToken       string
	SetupCompleted     bool
	TrialEndsAt        *time.Time
	SubscriptionPlan   string
	MustChangePassword bool
}

type Login struct {
	Users   userRepo.UserRepository
	Gyms    gymRepo.GymRepository
	UoW     sharedDomain.UnitOfWork
	Tokens  auth.TokenService
	Audit   audit.Recorder
	NowFunc func() time.Time
}

func NewLogin(users userRepo.UserRepository, gyms gymRepo.GymRepository,
	uow sharedDomain.UnitOfWork, tokens auth.TokenService, recorder audit.Recorder) *Login {
	return &Login{
		Users:   users,
		Gyms:    gyms,
		UoW:     uow,
		Tokens:  tokens,
		Audit:   recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

func (uc *Login) Execute(ctx context.Context, in LoginInput) (LoginOutput, error) {
	now := uc.NowFunc()
	var (
		userID     uuid.UUID
		gymID      uuid.UUID
		fullName   string
		email      string
		role       string
		gymName    *string
		setupDone  bool
		trialEnd   *time.Time
		plan       string
		mustChange bool
	)
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		user, err := uc.Users.GetByEmail(tx, in.Email)
		if err != nil {
			// Genericise to avoid leaking whether the email exists (UC-002 errors).
			return sharedDomain.NewBusinessError(userErrors.ErrInvalidCredentials, "")
		}
		if !user.Active {
			return sharedDomain.NewBusinessError(userErrors.ErrAccountInactive, "")
		}
		if err := auth.VerifyPassword(user.PasswordHash, in.Password); err != nil {
			return sharedDomain.NewBusinessError(userErrors.ErrInvalidCredentials, "")
		}
		gym, err := uc.Gyms.GetByID(tx, user.GymID)
		if err != nil {
			return err
		}
		user.MarkLoggedIn(now)
		if _, err := uc.Users.Update(tx, user); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       user.GymID,
			EntityType:  "users",
			EntityID:    user.ID,
			Action:      audit.ActionLogin,
			ActorUserID: &user.ID,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})

		userID = user.ID
		gymID = user.GymID
		fullName = user.FullName
		email = user.Email
		role = user.Role
		gymName = gym.Name
		setupDone = gym.IsSetupComplete()
		trialEnd = gym.TrialEndsAt
		plan = gym.SubscriptionPlan
		mustChange = user.MustChangePassword
		return nil
	})
	if err != nil {
		return LoginOutput{}, err
	}

	access, err := uc.Tokens.GenerateAccessToken(userID, gymID, role)
	if err != nil {
		return LoginOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	refresh, err := uc.Tokens.GenerateRefreshToken(userID, gymID, role)
	if err != nil {
		return LoginOutput{}, sharedDomain.NewUnexpectedError(err)
	}

	return LoginOutput{
		UserID:             userID,
		GymID:              gymID,
		FullName:           fullName,
		Email:              email,
		Role:               role,
		GymName:            gymName,
		AccessToken:        access,
		RefreshToken:       refresh,
		SetupCompleted:     setupDone,
		TrialEndsAt:        trialEnd,
		SubscriptionPlan:   plan,
		MustChangePassword: mustChange,
	}, nil
}
