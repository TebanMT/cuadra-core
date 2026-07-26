//go:build sidecar

package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	chkRepo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CheckinByFingerprintInput. Identification already happened upstream — the
// BiometricHub ran the sample through the tinta-bio helper (1:N against its
// cached gallery) and resolved ref → member_fingerprints → socio. This use
// case owns what comes AFTER the match: access decision, checkin record,
// audit and sync (UC-029 pasos 5+).
type CheckinByFingerprintInput struct {
	GymID    uuid.UUID
	MemberID uuid.UUID
	Now      time.Time
}

// CheckinByFingerprint runs the post-identification half of UC-029: evaluate
// access status + record the checkin (no operator — the socio se registra
// solo con el dedazo). "No identificado" never reaches this use case; the
// hub surfaces it as an event without touching the DB.
type CheckinByFingerprint struct {
	Members *memApp.MemberService
	Repo    chkRepo.CheckinRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
	// Gyms (opcional) → evaluar la vigencia en la zona horaria del gym.
	Gyms gymRepo.GymRepository
}

func NewCheckinByFingerprint(
	members *memApp.MemberService,
	repo chkRepo.CheckinRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *CheckinByFingerprint {
	return &CheckinByFingerprint{
		Members: members, Repo: repo, UoW: uow, Audit: recorder,
	}
}

// WithGyms cablea el repo de gyms para evaluar el acceso en el día local.
func (uc *CheckinByFingerprint) WithGyms(g gymRepo.GymRepository) *CheckinByFingerprint {
	uc.Gyms = g
	return uc
}

func (uc *CheckinByFingerprint) Execute(ctx context.Context, in CheckinByFingerprintInput) (*CheckinView, error) {
	if in.MemberID == uuid.Nil {
		return nil, sharedDomain.NewValidationError(chkErrors.ErrMemberRequired)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	var view *CheckinView
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		v, err := recordCheckin(ctx, tx, uc.Members, uc.Gyms, uc.Repo, uc.Audit,
			in.GymID, in.MemberID, "fingerprint", nil, now)
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
