package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	fpDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// DeleteFingerprintInput backs UC-028b — soft-delete all active fingerprints
// for a member before a re-enrollment. GymID is required for the cross-gym
// check (same guard as RegisterFingerprint).
type DeleteFingerprintInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	MemberID    uuid.UUID
}

// DeleteFingerprint implements UC-028b. Soft-deletes every active
// MemberFingerprint row for the member and records one audit entry per
// template. The frontend calls this immediately before opening the
// enrollment modal so the subsequent RegisterFingerprint finds no
// existing rows and proceeds normally.
type DeleteFingerprint struct {
	Members      memRepo.MemberRepository
	Fingerprints memRepo.FingerprintRepository
	UoW          sharedDomain.UnitOfWork
	Audit        audit.Recorder
}

func NewDeleteFingerprint(
	members memRepo.MemberRepository,
	fingerprints memRepo.FingerprintRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *DeleteFingerprint {
	return &DeleteFingerprint{
		Members: members, Fingerprints: fingerprints,
		UoW: uow, Audit: recorder,
	}
}

func (uc *DeleteFingerprint) Execute(ctx context.Context, in DeleteFingerprintInput) error {
	now := time.Now().UTC()

	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		m, err := uc.Members.GetByID(tx, in.MemberID)
		if err != nil {
			return err
		}
		if m.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
		}

		existing, err := uc.Fingerprints.ListByMember(tx, in.MemberID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if len(existing) == 0 {
			return sharedDomain.NewBusinessError(fpDomain.ErrFingerprintNotFound, "")
		}

		for _, fp := range existing {
			fp.SoftDelete(now)
			if _, err := uc.Fingerprints.Update(tx, fp); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			_ = uc.Audit.Record(ctx, tx, audit.Entry{
				GymID:       in.GymID,
				EntityType:  "member_fingerprints",
				EntityID:    fp.ID,
				Action:      audit.ActionDelete,
				ActorUserID: &in.ActorUserID,
				Changes: map[string]any{
					"member_id":       fp.MemberID.String(),
					"template_format": fp.TemplateFormat,
				},
				IPAddress: audit.IPFromContext(ctx),
				UserAgent: audit.UAFromContext(ctx),
				At:        now,
			})
		}
		return nil
	})
}
