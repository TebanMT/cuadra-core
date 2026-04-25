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

// UpdateMembershipTypeInput carries UC-011 edit fields. All scalar fields are
// always sent — partial updates would require pointer-of-everything which the
// API doesn't need yet.
type UpdateMembershipTypeInput struct {
	GymID                uuid.UUID
	ActorUserID          uuid.UUID
	TypeID               uuid.UUID
	Name                 string
	Price                float64
	DurationDays         int
	EnrollmentFee        float64
	MaintenanceFee       float64
	MaintenanceFrequency string
}

type UpdateMembershipType struct {
	Repo  memRepo.MembershipTypeRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewUpdateMembershipType(repo memRepo.MembershipTypeRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdateMembershipType {
	return &UpdateMembershipType{Repo: repo, UoW: uow, Audit: recorder}
}

func (uc *UpdateMembershipType) Execute(ctx context.Context, in UpdateMembershipTypeInput) (*mtDomain.MembershipType, error) {
	now := time.Now().UTC()
	var out *mtDomain.MembershipType
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		mt, err := uc.Repo.GetByID(tx, in.TypeID)
		if err != nil {
			return err
		}
		if mt.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeNotFound, "")
		}
		nameLower := strings.ToLower(strings.TrimSpace(in.Name))
		if nameLower != strings.ToLower(mt.Name) {
			exists, err := uc.Repo.ExistsByGymAndName(tx, in.GymID, in.Name)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if exists {
				return sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeNameDuplicate, "")
			}
		}
		before := map[string]any{
			"name": mt.Name, "price": mt.Price, "duration_days": mt.DurationDays,
		}
		if err := mt.Update(in.Name, in.Price, in.DurationDays, in.EnrollmentFee, in.MaintenanceFee, in.MaintenanceFrequency, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		updated, err := uc.Repo.Update(tx, mt)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "membership_types",
			EntityID:    updated.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"before": before,
				"after":  map[string]any{"name": updated.Name, "price": updated.Price, "duration_days": updated.DurationDays},
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
