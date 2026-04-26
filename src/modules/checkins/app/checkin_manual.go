package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	chkRepo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CheckinManualInput backs UC-030. The operator already picked the member from
// the search dropdown — by the time we hit the use case we have a concrete
// memberID, no search to do.
type CheckinManualInput struct {
	GymID      uuid.UUID
	OperatorID uuid.UUID
	MemberID   uuid.UUID
	Now        time.Time // optional — defaults to time.Now().UTC()
}

// CheckinManual is the operator-driven path. Used both by the standard
// checkin screen and as the fallback when fingerprint / PIN aren't available.
type CheckinManual struct {
	Members *memApp.MemberService
	Repo    chkRepo.CheckinRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
}

func NewCheckinManual(members *memApp.MemberService, repo chkRepo.CheckinRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CheckinManual {
	return &CheckinManual{Members: members, Repo: repo, UoW: uow, Audit: recorder}
}

func (uc *CheckinManual) Execute(ctx context.Context, in CheckinManualInput) (*CheckinView, error) {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	today := truncateToDay(now)
	op := in.OperatorID

	var view *CheckinView
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		v, err := recordCheckin(ctx, tx, uc.Members, uc.Repo, uc.Audit,
			in.GymID, in.MemberID, "manual", &op, now, today)
		if err != nil {
			return err
		}
		view = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}
