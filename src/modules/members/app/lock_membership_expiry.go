package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// LockMembershipExpiryMode picks how UC-017 interprets the input.
type LockMembershipExpiryMode int

const (
	// ModeExtendDays — "Extender vigencia [N] días". Days may be negative.
	ModeExtendDays LockMembershipExpiryMode = iota
	// ModeSetDate — "Establecer fecha [DD/MM/YYYY]".
	ModeSetDate
)

// LockMembershipExpiryInput backs UC-017.
type LockMembershipExpiryInput struct {
	GymID        uuid.UUID
	ActorUserID  uuid.UUID
	MembershipID uuid.UUID
	Mode         LockMembershipExpiryMode
	Days         int       // used when Mode == ModeExtendDays
	NewExpiry    time.Time // used when Mode == ModeSetDate
	Reason       string    // mandatory, ≥5 chars (DA-17.2)
}

type LockMembershipExpiryOutput struct {
	MembershipID   uuid.UUID
	PreviousExpiry time.Time
	NewExpiry      time.Time
	DaysAdded      int
}

type LockMembershipExpiry struct {
	Memberships memRepo.MembershipRepository
	Adjustments memRepo.MembershipAdjustmentRepository
	UoW         sharedDomain.UnitOfWork
	Audit       audit.Recorder
}

func NewLockMembershipExpiry(memberships memRepo.MembershipRepository, adjustments memRepo.MembershipAdjustmentRepository,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *LockMembershipExpiry {
	return &LockMembershipExpiry{
		Memberships: memberships,
		Adjustments: adjustments,
		UoW:         uow,
		Audit:       recorder,
	}
}

func (uc *LockMembershipExpiry) Execute(ctx context.Context, in LockMembershipExpiryInput) (*LockMembershipExpiryOutput, error) {
	now := time.Now().UTC()
	var out LockMembershipExpiryOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		ms, err := uc.Memberships.GetByID(tx, in.MembershipID)
		if err != nil {
			return err
		}
		if ms.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrMembershipNotFound, "")
		}
		var prev, next time.Time
		var daysAdded int
		switch in.Mode {
		case ModeExtendDays:
			p, n, err := ms.AdjustExpiry(in.Days, now)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			prev, next, daysAdded = p, n, in.Days
		case ModeSetDate:
			p, d, err := ms.SetExpiry(in.NewExpiry, now)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			// SetExpiry deja ms.ExpiryDate apuntando a la fecha nueva;
			// la deserferenciamos aquí. Si el caller intentó setearle
			// expiry a un pending_payment, ms.SetExpiry ya devolvió
			// error arriba.
			if ms.ExpiryDate == nil {
				return sharedDomain.NewValidationError(memErrors.ErrAdjustmentInvalidDays)
			}
			prev, next, daysAdded = p, *ms.ExpiryDate, d
		default:
			return sharedDomain.NewValidationError(memErrors.ErrAdjustmentInvalidDays)
		}

		// Build the adjustment row first so we get reason validation BEFORE
		// the UPDATE — though they're in the same UoW, an early return saves
		// a roundtrip.
		adj, err := membershipDomain.NewAdjustment(uuid.New(), in.GymID, ms.ID, in.ActorUserID,
			in.Reason, daysAdded, prev, next, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}

		if _, err := uc.Memberships.Update(tx, ms); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if _, err := uc.Adjustments.Create(tx, adj); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "memberships",
			EntityID:    ms.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"adjustment_id":   adj.ID,
				"previous_expiry": prev.Format("2006-01-02"),
				"new_expiry":      next.Format("2006-01-02"),
				"days_added":      daysAdded,
				"reason":          adj.Reason,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = LockMembershipExpiryOutput{
			MembershipID:   ms.ID,
			PreviousExpiry: prev,
			NewExpiry:      next,
			DaysAdded:      daysAdded,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
