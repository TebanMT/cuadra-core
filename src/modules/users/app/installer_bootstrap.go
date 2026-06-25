package app

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
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
	// OwnerWelcome (opcional) encola el banner "tu sistema ya está vivo" al
	// dueño en el primer link del primer dispositivo (ADR-010). Nil = no se
	// envía (tests / builds sin notificaciones).
	OwnerWelcome OwnerWelcomeNotifier
	NowFunc      func() time.Time
}

// WithOwnerWelcome cablea el notifier del banner de bienvenida del dueño.
func (uc *RedeemInstallerBootstrap) WithOwnerWelcome(n OwnerWelcomeNotifier) *RedeemInstallerBootstrap {
	uc.OwnerWelcome = n
	return uc
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

	// Primer link del primer dispositivo = el sistema queda "vivo". Si el gym
	// aún no recibió el banner de bienvenida del dueño, regeneramos su código
	// de acceso y lo enviamos por WhatsApp (ADR-010, opción A). Best-effort +
	// fire-once (flag del gym); un fallo NO rompe el redeem. Sólo cuando el
	// dispositivo realmente quedó vinculado (hay sidecar token).
	if uc.OwnerWelcome != nil && out.SidecarToken != "" {
		if newHash, ok := uc.welcomeOwnerOnFirstLink(ctx, bs.GymID, bs.UserID); ok {
			out.PinHash = newHash
			out.HasPIN = true
		}
	}
	return out, nil
}

// welcomeOwnerOnFirstLink regenera el código de acceso del dueño y encola el
// banner "tu sistema ya está vivo", UNA sola vez por gym (flag
// gym.OwnerWelcomeSent). Devuelve el nuevo pin_hash para que el desktop lo
// espeje (login-pin offline) — vacío/false si se saltó o falló.
func (uc *RedeemInstallerBootstrap) welcomeOwnerOnFirstLink(ctx context.Context, gymID, ownerUserID uuid.UUID) (string, bool) {
	now := uc.NowFunc()
	var newHash string
	var done bool
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		gym, err := uc.Gyms.GetByID(tx, gymID)
		if err != nil {
			return err
		}
		if gym.OwnerWelcomeSent() {
			return nil // fire-once: ya se envió para este gym
		}
		user, err := uc.Users.GetByID(tx, ownerUserID)
		if err != nil {
			return err
		}
		if user.Role != userDomain.RoleOwner {
			return nil // sólo el dueño recibe este banner (bootstrap = owner)
		}
		phone := ""
		if user.Phone != nil {
			phone = strings.TrimSpace(*user.Phone)
		}
		if phone == "" {
			// Sin teléfono no hay a quién enviar: NO regeneramos el código
			// (evita dejar al dueño sin saber su PIN nuevo) ni marcamos el
			// flag, por si agrega teléfono y re-linkea más adelante.
			return nil
		}
		// Regenerar el código de acceso del dueño (opción A) — el plaintext
		// se hornea en el banner; en BD sólo queda el hash.
		pin, err := auth.GenerateTempPIN()
		if err != nil {
			return err
		}
		h, err := auth.HashPIN(pin)
		if err != nil {
			return err
		}
		user.AssignPIN(h, now)
		if _, err := uc.Users.Update(tx, user); err != nil {
			return err
		}
		// Encolar el banner por WhatsApp (atómico con el cambio de PIN).
		if _, err := uc.OwnerWelcome.Notify(ctx, tx, OwnerWelcomeNotifyInput{
			GymID: gymID, UserID: ownerUserID, PIN: pin,
		}, now); err != nil {
			return err
		}
		// Marcar fire-once.
		gym.MarkOwnerWelcomeSent(now)
		if _, err := uc.Gyms.Update(tx, gym); err != nil {
			return err
		}
		newHash = h
		done = true
		return nil
	})
	if err != nil {
		log.Printf("[redeem] owner welcome failed gym=%s: %v", gymID, err)
		return "", false
	}
	return newHash, done
}
