// Package app — gyms use cases (UC-001 wizard steps 2-5, UC-005).
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymErrors "github.com/cuadra/cuadra-core/src/modules/gyms/domain/errors"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// UpdateBasicInfoInput is UC-001 step 2 (PATCH /api/v1/gyms/me/setup).
type UpdateBasicInfoInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	Name        string
	City        string
	WhatsApp    string
}

type UpdateBasicInfo struct {
	Gyms  gymRepo.GymRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewUpdateBasicInfo(gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdateBasicInfo {
	return &UpdateBasicInfo{Gyms: gyms, UoW: uow, Audit: recorder}
}

func (uc *UpdateBasicInfo) Execute(ctx context.Context, in UpdateBasicInfoInput) (*gymDomain.Gym, error) {
	var out *gymDomain.Gym
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		g, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return err
		}
		// Chequeo de duplicado ANTES de mutar el agregado — si el número
		// ya está en otro gym vivo, devolvemos un error de negocio claro
		// para que el wizard del dashboard lo muestre bajo el campo.
		// Excluimos el propio gym del lookup: re-guardar el mismo número
		// (idempotencia del PATCH) no debe colisionar.
		if in.WhatsApp != "" {
			taken, err := uc.Gyms.ExistsByWhatsApp(tx, in.WhatsApp, in.GymID)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if taken {
				return sharedDomain.NewBusinessError(gymErrors.ErrWhatsAppAlreadyTaken, "")
			}
		}
		if err := g.UpdateBasicInfo(in.Name, in.City, in.WhatsApp); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		updated, err := uc.Gyms.Update(tx, g)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       g.ID,
			EntityType:  "gyms",
			EntityID:    g.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"name": in.Name, "city": in.City, "whatsapp": in.WhatsApp},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          time.Now().UTC(),
		})
		out = updated
		return nil
	})
	return out, err
}

// UpdatePaymentMethodsInput is UC-001 step 4.
type UpdatePaymentMethodsInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	Methods     []string
}

type UpdatePaymentMethods struct {
	Gyms  gymRepo.GymRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewUpdatePaymentMethods(gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdatePaymentMethods {
	return &UpdatePaymentMethods{Gyms: gyms, UoW: uow, Audit: recorder}
}

func (uc *UpdatePaymentMethods) Execute(ctx context.Context, in UpdatePaymentMethodsInput) (*gymDomain.Gym, error) {
	var out *gymDomain.Gym
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		g, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return err
		}
		if err := g.UpdatePaymentMethods(in.Methods); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		updated, err := uc.Gyms.Update(tx, g)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       g.ID,
			EntityType:  "gyms",
			EntityID:    g.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"payment_methods": in.Methods},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          time.Now().UTC(),
		})
		out = updated
		return nil
	})
	return out, err
}

// CompleteSetup is UC-001 step 5 (POST /api/v1/gyms/me/setup/complete).
type CompleteSetup struct {
	Gyms  gymRepo.GymRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewCompleteSetup(gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CompleteSetup {
	return &CompleteSetup{Gyms: gyms, UoW: uow, Audit: recorder}
}

func (uc *CompleteSetup) Execute(ctx context.Context, gymID, actorUserID uuid.UUID) (*gymDomain.Gym, error) {
	now := time.Now().UTC()
	var out *gymDomain.Gym
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		g, err := uc.Gyms.GetByID(tx, gymID)
		if err != nil {
			return err
		}
		if g.IsSetupComplete() {
			return sharedDomain.NewBusinessError(gymErrors.ErrGymAlreadySetup, "")
		}
		hasMT, err := uc.Gyms.HasMembershipType(tx, gymID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if err := g.CompleteSetup(hasMT, now); err != nil {
			return sharedDomain.NewBusinessError(err, "")
		}
		updated, err := uc.Gyms.Update(tx, g)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       gymID,
			EntityType:  "gyms",
			EntityID:    gymID,
			Action:      audit.ActionUpdate,
			ActorUserID: &actorUserID,
			Changes:     map[string]any{"setup_completed_at": now},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = updated
		return nil
	})
	return out, err
}

// UpdateProfileInput is UC-005 (PATCH /api/v1/gyms/me).
type UpdateProfileInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	Update      gymDomain.ProfileUpdate
}

type UpdateProfile struct {
	Gyms  gymRepo.GymRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewUpdateProfile(gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdateProfile {
	return &UpdateProfile{Gyms: gyms, UoW: uow, Audit: recorder}
}

func (uc *UpdateProfile) Execute(ctx context.Context, in UpdateProfileInput) (*gymDomain.Gym, error) {
	var out *gymDomain.Gym
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		g, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return err
		}
		// Si el PATCH incluye whatsapp y NO es el mismo del gym actual,
		// chequear duplicado entre otros gyms vivos. Si el dueño re-envía
		// el mismo número o lo limpia (string vacío) saltamos la consulta.
		if in.Update.WhatsApp != nil {
			candidate := *in.Update.WhatsApp
			isSame := g.WhatsApp != nil && *g.WhatsApp == candidate
			if candidate != "" && !isSame {
				taken, err := uc.Gyms.ExistsByWhatsApp(tx, candidate, in.GymID)
				if err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				if taken {
					return sharedDomain.NewBusinessError(gymErrors.ErrWhatsAppAlreadyTaken, "")
				}
			}
		}
		if err := g.ApplyProfileUpdate(in.Update); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		updated, err := uc.Gyms.Update(tx, g)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       g.ID,
			EntityType:  "gyms",
			EntityID:    g.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes:     in.Update,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          time.Now().UTC(),
		})
		out = updated
		return nil
	})
	return out, err
}
