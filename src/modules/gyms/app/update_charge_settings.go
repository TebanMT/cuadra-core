package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// UpdateChargeSettingsInput sustenta el endpoint PATCH /api/v1/gyms/me/charge-settings.
// Cada campo es puntero: nil = "no cambiar"; valor presente (incluido
// false o 0) = "fijar a este valor". Permite parchear sólo lo que cambia
// sin pisar el resto del JSON.
type UpdateChargeSettingsInput struct {
	GymID                uuid.UUID
	ActorUserID          uuid.UUID
	ChargesEnrollment    *bool
	ChargesMaintenance   *bool
	EnrollmentAmount     *float64
	MaintenanceAmount    *float64
	MaintenanceFrequency *string
}

type UpdateChargeSettings struct {
	Gyms  gymRepo.GymRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewUpdateChargeSettings(gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdateChargeSettings {
	return &UpdateChargeSettings{Gyms: gyms, UoW: uow, Audit: recorder}
}

func (uc *UpdateChargeSettings) Execute(ctx context.Context, in UpdateChargeSettingsInput) (*gymDomain.Gym, error) {
	var out *gymDomain.Gym
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		g, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := g.UpdateChargeSettings(gymDomain.ChargeSettingsInput{
			ChargesEnrollment:    in.ChargesEnrollment,
			ChargesMaintenance:   in.ChargesMaintenance,
			EnrollmentAmount:     in.EnrollmentAmount,
			MaintenanceAmount:    in.MaintenanceAmount,
			MaintenanceFrequency: in.MaintenanceFrequency,
		}, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		updated, err := uc.Gyms.Update(tx, g)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// Audit con el set completo resultante, no sólo el patch — así
		// el historial muestra el estado en cada momento sin depender
		// del diff de versiones previas.
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       g.ID,
			EntityType:  "gyms",
			EntityID:    g.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"charge_settings": g.ChargeSettings,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = updated
		return nil
	})
	return out, err
}
