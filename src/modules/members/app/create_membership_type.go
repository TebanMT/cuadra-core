// Package app implements the use cases of the `members` BC.
//
// UC-011 — MembershipType CRUD
// UC-012 — Crear socio
// UC-013 — Editar socio
// UC-014 — Listar / buscar / filtrar socios
// UC-015 — Ver detalle de socio
// UC-016 — Cambiar estado activo / inactivo
// UC-017 — Bloquear / ajustar vencimiento manual
// UC-032 — Asignar PIN al socio (operación, el flujo de check-in vive en checkins)
package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ---------------------------------------------------------------------------
// CreateMembershipType — UC-011
// ---------------------------------------------------------------------------

type CreateMembershipTypeInput struct {
	GymID                uuid.UUID
	ActorUserID          uuid.UUID
	Name                 string
	Price                float64
	DurationDays         int
	EnrollmentFee        float64
	MaintenanceFee       float64
	MaintenanceFrequency string
}

type CreateMembershipType struct {
	Repo  memRepo.MembershipTypeRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewCreateMembershipType(repo memRepo.MembershipTypeRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CreateMembershipType {
	return &CreateMembershipType{Repo: repo, UoW: uow, Audit: recorder}
}

func (uc *CreateMembershipType) Execute(ctx context.Context, in CreateMembershipTypeInput) (*mtDomain.MembershipType, error) {
	now := time.Now().UTC()
	id := uuid.New()
	mt, err := mtDomain.New(id, in.GymID, in.Name, in.Price, in.DurationDays,
		in.EnrollmentFee, in.MaintenanceFee, in.MaintenanceFrequency, now)
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}

	var out *mtDomain.MembershipType
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		exists, err := uc.Repo.ExistsByGymAndName(tx, in.GymID, strings.TrimSpace(in.Name))
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if exists {
			return sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeNameDuplicate, "")
		}
		created, err := uc.Repo.Create(tx, mt)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "membership_types",
			EntityID:    created.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"name": created.Name, "price": created.Price, "duration_days": created.DurationDays,
				"enrollment_fee": created.EnrollmentFee, "maintenance_fee": created.MaintenanceFee,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = created
		return nil
	})
	return out, err
}
