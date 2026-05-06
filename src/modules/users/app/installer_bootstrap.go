package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/installerbootstrap"
)

// IssueInstallerBootstrap is called by the dashboard right after the
// owner finishes web signup. It mints a single-use plaintext code that
// the desktop's first launch can swap for a full session — operator JWTs
// + sk_live_* sidecar credential — without re-prompting for credentials.
type IssueInstallerBootstrap struct {
	Store installerbootstrap.Store
}

func NewIssueInstallerBootstrap(store installerbootstrap.Store) *IssueInstallerBootstrap {
	return &IssueInstallerBootstrap{Store: store}
}

type IssueInstallerBootstrapInput struct {
	GymID  uuid.UUID
	UserID uuid.UUID
}

type IssueInstallerBootstrapOutput struct {
	Token     string
	ExpiresAt time.Time
}

func (uc *IssueInstallerBootstrap) Execute(ctx context.Context, in IssueInstallerBootstrapInput) (IssueInstallerBootstrapOutput, error) {
	if uc.Store == nil {
		return IssueInstallerBootstrapOutput{}, errors.New("installer bootstrap store not wired")
	}
	if in.GymID == uuid.Nil || in.UserID == uuid.Nil {
		return IssueInstallerBootstrapOutput{}, errors.New("installer bootstrap: missing ids")
	}
	plain, hash, err := installerbootstrap.Generate()
	if err != nil {
		return IssueInstallerBootstrapOutput{}, err
	}
	exp := time.Now().UTC().Add(installerbootstrap.DefaultTTL)
	if _, err := uc.Store.Insert(ctx, in.GymID, in.UserID, hash, exp); err != nil {
		return IssueInstallerBootstrapOutput{}, err
	}
	return IssueInstallerBootstrapOutput{Token: plain, ExpiresAt: exp}, nil
}

// RedeemInstallerBootstrap is the cloud handler for the desktop's
// /api/v1/auth/redeem-installer call. Same shape as a successful Login but
// authenticated by the bootstrap code instead of email + password. Also
// invokes BootstrapSidecarToken so the desktop receives its sk_live_* in
// the same response.
type RedeemInstallerBootstrap struct {
	Store       installerbootstrap.Store
	Users       userRepo.UserRepository
	Gyms        gymRepo.GymRepository
	UoW         sharedDomain.UnitOfWork
	Tokens      auth.TokenService
	SidecarBoot *BootstrapSidecarToken
	NowFunc     func() time.Time
}

func NewRedeemInstallerBootstrap(
	store installerbootstrap.Store,
	users userRepo.UserRepository,
	gyms gymRepo.GymRepository,
	uow sharedDomain.UnitOfWork,
	tokens auth.TokenService,
	sidecarBoot *BootstrapSidecarToken,
) *RedeemInstallerBootstrap {
	return &RedeemInstallerBootstrap{
		Store: store, Users: users, Gyms: gyms, UoW: uow, Tokens: tokens, SidecarBoot: sidecarBoot,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type RedeemInstallerBootstrapInput struct {
	Token       string
	ClientID    uuid.UUID
	DeviceLabel string
}

type RedeemInstallerBootstrapOutput struct {
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
	SidecarToken     string
}

func (uc *RedeemInstallerBootstrap) Execute(ctx context.Context, in RedeemInstallerBootstrapInput) (RedeemInstallerBootstrapOutput, error) {
	if uc.Store == nil {
		return RedeemInstallerBootstrapOutput{}, errors.New("installer bootstrap store not wired")
	}
	if in.Token == "" {
		return RedeemInstallerBootstrapOutput{}, installerbootstrap.ErrNotFound
	}
	now := uc.NowFunc()
	hash := installerbootstrap.Hash(in.Token)
	bs, err := uc.Store.Redeem(ctx, hash, now)
	if err != nil {
		return RedeemInstallerBootstrapOutput{}, err
	}

	var (
		fullName, email, role, plan string
		setupDone                   bool
		trialEnd                    *time.Time
		gymName                     *string
	)
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		user, err := uc.Users.GetByID(tx, bs.UserID)
		if err != nil {
			return err
		}
		gym, err := uc.Gyms.GetByID(tx, bs.GymID)
		if err != nil {
			return err
		}
		fullName = user.FullName
		email = user.Email
		role = user.Role
		setupDone = gym.IsSetupComplete()
		trialEnd = gym.TrialEndsAt
		plan = gym.SubscriptionPlan
		gymName = gym.Name
		user.MarkLoggedIn(now)
		_, _ = uc.Users.Update(tx, user)
		return nil
	})
	if err != nil {
		return RedeemInstallerBootstrapOutput{}, err
	}

	access, err := uc.Tokens.GenerateAccessToken(bs.UserID, bs.GymID, role)
	if err != nil {
		return RedeemInstallerBootstrapOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	refresh, err := uc.Tokens.GenerateRefreshToken(bs.UserID, bs.GymID, role)
	if err != nil {
		return RedeemInstallerBootstrapOutput{}, sharedDomain.NewUnexpectedError(err)
	}

	out := RedeemInstallerBootstrapOutput{
		UserID:           bs.UserID,
		GymID:            bs.GymID,
		FullName:         fullName,
		Email:            email,
		Role:             role,
		GymName:          gymName,
		AccessToken:      access,
		RefreshToken:     refresh,
		SetupCompleted:   setupDone,
		TrialEndsAt:      trialEnd,
		SubscriptionPlan: plan,
	}
	if uc.SidecarBoot != nil && in.ClientID != uuid.Nil {
		tok, err := uc.SidecarBoot.Execute(ctx, BootstrapSidecarTokenInput{
			GymID:       bs.GymID,
			UserID:      bs.UserID,
			ClientID:    in.ClientID,
			DeviceLabel: in.DeviceLabel,
		})
		if err == nil {
			out.SidecarToken = tok
		}
	}
	return out, nil
}
