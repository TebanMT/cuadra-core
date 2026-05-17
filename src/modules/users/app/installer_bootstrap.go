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
	now := time.Now().UTC()
	// Invalida cualquier código previo aún sin canjear para este
	// (gym, user). Antes podían coexistir N códigos válidos en paralelo;
	// el dueño esperaba que "Regenerar" reemplace, no que acumule. Si
	// alguien pasó por SupabRedeem antes del Insert, fail silencioso —
	// el peor caso es seguir con la semántica vieja, no romper.
	_, _ = uc.Store.ExpirePriorActive(ctx, in.GymID, in.UserID, now)
	plain, hash, err := installerbootstrap.Generate()
	if err != nil {
		return IssueInstallerBootstrapOutput{}, err
	}
	exp := now.Add(installerbootstrap.DefaultTTL)
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
	Phone            string
	Role             string
	GymName          *string
	AccessToken      string
	RefreshToken     string
	SetupCompleted   bool
	TrialEndsAt      *time.Time
	SubscriptionPlan string
	SidecarToken     string
	// HasPIN: el bootstrap se redime POSTERIOR al signup en cloud (donde
	// el dueño ya recibió un PIN auto-asignado). Sin esto, el desktop
	// arrancaba con has_pin=false y pintaba "Crear PIN" en perfil para
	// un dueño que ya tenía pin_hash en DB.
	HasPIN bool
	// PinHash viaja para que el sidecar pueda mirrorear el bcrypt al
	// sqlite local y el flujo login-pin offline funcione desde t=0 (sin
	// esperar el primer sync pull). Mismo tier que password_hash.
	PinHash string
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
		fullName, email, phone, role, plan string
		setupDone                          bool
		trialEnd                           *time.Time
		gymName                            *string
		hasPIN                             bool
		pinHash                            string
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
		if user.Phone != nil {
			phone = *user.Phone
		}
		role = user.Role
		setupDone = gym.IsSetupComplete()
		trialEnd = gym.TrialEndsAt
		plan = gym.SubscriptionPlan
		gymName = gym.Name
		hasPIN = user.HasPIN()
		if user.PinHash != nil {
			pinHash = *user.PinHash
		}
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
		Phone:            phone,
		Role:             role,
		GymName:          gymName,
		AccessToken:      access,
		RefreshToken:     refresh,
		SetupCompleted:   setupDone,
		TrialEndsAt:      trialEnd,
		SubscriptionPlan: plan,
		HasPIN:           hasPIN,
		PinHash:          pinHash,
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
