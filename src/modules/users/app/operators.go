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

// CreateOperatorInput is UC-006 (POST /api/v1/users). Owner-only — the
// handler enforces that via middleware.
//
// Wire shape (PIN-first model):
//   - Phone is REQUIRED. El PIN del operador se entrega por WhatsApp, así
//     que sin teléfono no podemos dar de alta. La unicidad es por gym (la
//     misma persona puede trabajar en dos gyms con el mismo número).
//   - Email is OPTIONAL. El operador rara vez tiene correo; si no hay, se
//     guarda string vacío y el índice único lo ignora.
//   - No Password field. CreateOperator genera el PIN automáticamente,
//     lo hashea, y devuelve el plaintext UNA vez en la respuesta para que
//     el FE lo muestre como fallback si WhatsApp no está conectado.
type CreateOperatorInput struct {
	GymID    uuid.UUID
	OwnerID  uuid.UUID
	FullName string
	Phone    string
	Email    *string // nil o "" -> sin correo
}

// CreateOperatorOutput viaja al FE como wire-shape directo (auth_controller
// lo serializa en snake_case). El campo PIN va sólo en la respuesta 201; el
// audit row NUNCA logea el plaintext.
type CreateOperatorOutput struct {
	UserID           uuid.UUID
	FullName         string
	Phone            string
	Email            string
	PIN              string
	WhatsAppDelivery bool   // true = enqueued; false = skip silencioso
	WhatsAppSkipped  string // razón cuando WhatsAppDelivery=false ("whatsapp_not_connected", "no_user_phone", …)
}

type CreateOperator struct {
	Users    userRepo.UserRepository
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
	Notifier OperatorWelcomePINNotifier
	// Gyms (opcional). Cuando está cableado, el cap de operadores depende
	// del SKU del gym: Standard = MaxOperatorsStandard, Plus/Trial =
	// MaxOperatorsPerGym. Sin Gyms (tests), cae al cap absoluto.
	Gyms    gymRepo.GymRepository
	NowFunc func() time.Time
}

func NewCreateOperator(users userRepo.UserRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CreateOperator {
	return &CreateOperator{
		Users:    users,
		UoW:      uow,
		Audit:    recorder,
		Notifier: noopOperatorWelcomePINNotifier{},
		NowFunc:  func() time.Time { return time.Now().UTC() },
	}
}

// WithWelcomePINNotifier wires the WhatsApp seam used to deliver the
// generated PIN. Idempotent — el main.go puede llamarlo o no; el default
// noop devuelve "notifier_not_wired" y CreateOperator NO falla por eso.
func (uc *CreateOperator) WithWelcomePINNotifier(n OperatorWelcomePINNotifier) *CreateOperator {
	if n != nil {
		uc.Notifier = n
	}
	return uc
}

// WithGymRepo cables el repo de gym para hacer el cap dependiente del SKU
// (Standard vs Plus). Idempotente — sin él, el cap es el absoluto del
// producto. main.go lo inyecta; los tests pueden ignorarlo.
func (uc *CreateOperator) WithGymRepo(g gymRepo.GymRepository) *CreateOperator {
	if g != nil {
		uc.Gyms = g
	}
	return uc
}

func (uc *CreateOperator) Execute(ctx context.Context, in CreateOperatorInput) (CreateOperatorOutput, error) {
	pin, err := auth.GenerateTempPIN()
	if err != nil {
		return CreateOperatorOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	pinHash, err := auth.HashPIN(pin)
	if err != nil {
		return CreateOperatorOutput{}, sharedDomain.NewUnexpectedError(err)
	}

	now := uc.NowFunc()
	userID := uuid.New()
	// NewOperator centraliza la validación de phone-obligatorio + email
	// opcional + normalización. password_hash queda "" → la columna NULL.
	user, err := userDomain.NewOperator(userID, in.GymID, in.FullName, in.Phone, in.Email, "", in.OwnerID, now)
	if err != nil {
		return CreateOperatorOutput{}, sharedDomain.NewValidationError(err)
	}
	user.AssignPIN(pinHash, now)
	// AssignPIN bumps Version a 2. Lo dejamos en 1 — el row recién creado.
	user.Version = 1

	dispatch := OperatorWelcomePINDispatch{}
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		if user.Email != "" {
			exists, err := uc.Users.ExistsByEmail(tx, user.Email)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if exists {
				return sharedDomain.NewBusinessError(userErrors.ErrEmailAlreadyExists, "")
			}
		}
		if user.Phone != nil {
			exists, err := uc.Users.ExistsByPhoneInGym(tx, in.GymID, *user.Phone, nil)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if exists {
				return sharedDomain.NewBusinessError(userErrors.ErrPhoneTakenInGym, "")
			}
		}
		count, err := uc.Users.CountOperatorsByGym(tx, in.GymID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// Cap dependiente del SKU. MaxOperatorsStandard - 1 = 1 operador
		// activo además del dueño en Standard; Plus/Trial usa el cap del
		// producto (MaxOperatorsPerGym - 1 = 10). Si Gyms no está cableado
		// (tests), cae al cap absoluto.
		cap := userDomain.MaxOperatorsPerGym - 1
		standardCap := false
		if uc.Gyms != nil {
			g, gerr := uc.Gyms.GetByID(tx, in.GymID)
			if gerr == nil && g != nil && !gymDomain.CanAccessPlusFeatures(g.SubscriptionPlan) {
				cap = userDomain.MaxOperatorsStandard - 1
				standardCap = true
			}
		}
		if count >= cap {
			if standardCap {
				return sharedDomain.NewBusinessError(userErrors.ErrOperatorLimitStandard, "")
			}
			return sharedDomain.NewBusinessError(userErrors.ErrOperatorLimitReached, "")
		}
		if _, err := uc.Users.Create(tx, user); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// Audit registra el alta — pin_assigned: true sin el plaintext.
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "users",
			EntityID:    user.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.OwnerID,
			Changes: map[string]any{
				"role":         "operator",
				"email":        user.Email,
				"phone":        derefString(user.Phone),
				"pin_assigned": true,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		// Notifier corre dentro de la misma tx — el enqueue es atómico
		// con el INSERT del user. Si el gym no tiene WhatsApp, el notifier
		// retorna Dispatched=false con SkippedReason; eso NO es error.
		res, nerr := uc.Notifier.Notify(ctx, tx, OperatorWelcomePINNotifyInput{
			GymID:  in.GymID,
			UserID: user.ID,
			PIN:    pin,
		}, now)
		if nerr != nil {
			return nerr
		}
		dispatch = res
		return nil
	})
	if err != nil {
		return CreateOperatorOutput{}, err
	}
	return CreateOperatorOutput{
		UserID:           user.ID,
		FullName:         user.FullName,
		Phone:            derefString(user.Phone),
		Email:            user.Email,
		PIN:              pin,
		WhatsAppDelivery: dispatch.Dispatched,
		WhatsAppSkipped:  dispatch.SkippedReason,
	}, nil
}

// UpdateOperatorInput is UC-007 (PATCH /api/v1/users/{id}). Only the editable
// surface — role/active/password/pin live in their own use cases.
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
		// Email puede llegar como "" → "borrar correo": UpdateProfile lo
		// permite para operators y bloquea para owners. Sólo chequeamos
		// unicidad cuando el valor nuevo es no-vacío y distinto al actual.
		if in.Email != nil {
			cleanNew := normalizeEmailForCheck(*in.Email)
			if cleanNew != "" && cleanNew != u.Email {
				exists, err := uc.Users.ExistsByEmail(tx, cleanNew)
				if err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				if exists {
					return sharedDomain.NewBusinessError(userErrors.ErrEmailAlreadyExists, "")
				}
			}
		}
		// Phone: si viene en el payload, validar unicidad dentro del gym
		// (excluyendo el propio target — un PATCH que repite el mismo
		// número no debe colisionar consigo mismo).
		if in.Phone != nil {
			cleanNew := userDomain.NormalizePhone(*in.Phone)
			if cleanNew != "" {
				exists, err := uc.Users.ExistsByPhoneInGym(tx, in.GymID, cleanNew, &u.ID)
				if err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				if exists {
					return sharedDomain.NewBusinessError(userErrors.ErrPhoneTakenInGym, "")
				}
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
// Sigue existiendo para operadores legacy con email+password; el flujo nuevo
// usa RotateOperatorPIN.
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

// RotateOperatorPINInput es el caso "el dueño le regenera el PIN al
// operador". Se diferencia de AssignSelfPIN (usuario autenticado cambia
// SU PIN) en que sólo el dueño puede dispararlo y dispara la entrega
// por WhatsApp del nuevo código.
type RotateOperatorPINInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	TargetID    uuid.UUID
}

type RotateOperatorPINOutput struct {
	UserID           uuid.UUID
	PIN              string
	WhatsAppDelivery bool
	WhatsAppSkipped  string
}

type RotateOperatorPIN struct {
	Users    userRepo.UserRepository
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
	Notifier OperatorWelcomePINNotifier
	NowFunc  func() time.Time
}

func NewRotateOperatorPIN(users userRepo.UserRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *RotateOperatorPIN {
	return &RotateOperatorPIN{
		Users:    users,
		UoW:      uow,
		Audit:    recorder,
		Notifier: noopOperatorWelcomePINNotifier{},
		NowFunc:  func() time.Time { return time.Now().UTC() },
	}
}

func (uc *RotateOperatorPIN) WithWelcomePINNotifier(n OperatorWelcomePINNotifier) *RotateOperatorPIN {
	if n != nil {
		uc.Notifier = n
	}
	return uc
}

func (uc *RotateOperatorPIN) Execute(ctx context.Context, in RotateOperatorPINInput) (RotateOperatorPINOutput, error) {
	pin, err := auth.GenerateTempPIN()
	if err != nil {
		return RotateOperatorPINOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	pinHash, err := auth.HashPIN(pin)
	if err != nil {
		return RotateOperatorPINOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	now := uc.NowFunc()
	dispatch := OperatorWelcomePINDispatch{}
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		u, err := uc.Users.GetByID(tx, in.TargetID)
		if err != nil {
			return err
		}
		if u.GymID != in.GymID {
			return sharedDomain.NewBusinessError(userErrors.ErrCrossGym, "")
		}
		if !u.IsOperator() {
			// Rotación es sólo para operadores. Si el dueño quiere cambiar
			// su propio PIN, usa AssignSelfPIN — RotateOperatorPIN está
			// pensado como "the owner resets the receptionist's code".
			return sharedDomain.NewBusinessError(userErrors.ErrTargetNotEligible, "")
		}
		u.AssignPIN(pinHash, now)
		if _, err := uc.Users.Update(tx, u); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "users",
			EntityID:    in.TargetID,
			Action:      audit.ActionRotateOperatorPIN,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"pin_rotated": true},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		res, nerr := uc.Notifier.Notify(ctx, tx, OperatorWelcomePINNotifyInput{
			GymID:  in.GymID,
			UserID: u.ID,
			PIN:    pin,
		}, now)
		if nerr != nil {
			return nerr
		}
		dispatch = res
		return nil
	})
	if err != nil {
		return RotateOperatorPINOutput{}, err
	}
	return RotateOperatorPINOutput{
		UserID:           in.TargetID,
		PIN:              pin,
		WhatsAppDelivery: dispatch.Dispatched,
		WhatsAppSkipped:  dispatch.SkippedReason,
	}, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func normalizeEmailForCheck(s string) string {
	return userDomain.NormalizeEmail(s)
}
