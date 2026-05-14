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

// LoginPINInput is the sidecar's offline-validatable counterpart to LoginInput.
// User identity is by id (selected from the operators grid the desktop renders
// without authentication), not email — the kiosk UI hides email from the
// operator on purpose so PIN entry stays tap-only.
//
// This use case runs *only on the sidecar*. Cloud has no /auth/login-pin
// endpoint: cloud already has email+password, and exposing PIN-by-id to the
// public internet would invite enumeration of (gym → operators). The
// sidecar's /auth/operators endpoint that lists ids+names is bound to the
// LAN trust boundary (the desktop physically inside the gym).
type LoginPINInput struct {
	UserID uuid.UUID
	PIN    string
}

type LoginPINOutput struct {
	UserID           uuid.UUID
	GymID            uuid.UUID
	FullName         string
	Email            string
	Role             string
	GymName          *string
	AccessToken      string
	RefreshToken     string
	SetupCompleted   bool
	TrialEndsAt      *time.Time
	SubscriptionPlan string
}

type LoginPIN struct {
	Users   userRepo.UserRepository
	Gyms    gymRepo.GymRepository
	UoW     sharedDomain.UnitOfWork
	Tokens  auth.TokenService
	Audit   audit.Recorder
	NowFunc func() time.Time
}

func NewLoginPIN(users userRepo.UserRepository, gyms gymRepo.GymRepository,
	uow sharedDomain.UnitOfWork, tokens auth.TokenService, recorder audit.Recorder) *LoginPIN {
	return &LoginPIN{
		Users:   users,
		Gyms:    gyms,
		UoW:     uow,
		Tokens:  tokens,
		Audit:   recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

func (uc *LoginPIN) Execute(ctx context.Context, in LoginPINInput) (LoginPINOutput, error) {
	now := uc.NowFunc()
	var out LoginPINOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		user, err := uc.Users.GetByID(tx, in.UserID)
		if err != nil {
			// Same opaque error shape as LoginInput so we don't leak whether
			// the user_id exists — the desktop renders this as
			// "PIN incorrecto" either way.
			return sharedDomain.NewBusinessError(userErrors.ErrInvalidPINLogin, "")
		}
		if !user.Active {
			return sharedDomain.NewBusinessError(userErrors.ErrAccountInactive, "")
		}
		if !user.HasPIN() {
			return sharedDomain.NewBusinessError(userErrors.ErrPINNotSet, "")
		}
		if err := auth.VerifyPIN(*user.PinHash, in.PIN); err != nil {
			return sharedDomain.NewBusinessError(userErrors.ErrInvalidPINLogin, "")
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
			Action:      audit.ActionLoginPIN,
			ActorUserID: &user.ID,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out.UserID = user.ID
		out.GymID = user.GymID
		out.FullName = user.FullName
		out.Email = user.Email
		out.Role = user.Role
		out.GymName = gym.Name
		out.SetupCompleted = gym.IsSetupComplete()
		out.TrialEndsAt = gym.TrialEndsAt
		out.SubscriptionPlan = gym.SubscriptionPlan
		return nil
	})
	if err != nil {
		return LoginPINOutput{}, err
	}
	access, err := uc.Tokens.GenerateAccessToken(out.UserID, out.GymID, out.Role)
	if err != nil {
		return LoginPINOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	refresh, err := uc.Tokens.GenerateRefreshToken(out.UserID, out.GymID, out.Role)
	if err != nil {
		return LoginPINOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	out.AccessToken = access
	out.RefreshToken = refresh
	return out, nil
}
