// Package app — members use cases. Sesión 1 implements only
// CreateMembershipType (UC-001 step 3 dependency). Full CRUD lands in Sesión 2.
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
			Changes:     map[string]any{"name": created.Name, "price": created.Price, "duration_days": created.DurationDays},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = created
		return nil
	})
	return out, err
}
