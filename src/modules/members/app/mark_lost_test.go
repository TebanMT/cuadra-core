package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/members/app"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type markLostRepo struct {
	memRepo.MemberRepository
	record  *memRepo.MemberWithMembership
	updates int
}

func (r *markLostRepo) GetWithCurrentMembership(sharedDomain.Transaction, uuid.UUID, uuid.UUID) (*memRepo.MemberWithMembership, error) {
	return r.record, nil
}
func (r *markLostRepo) Update(_ sharedDomain.Transaction, m *memberDomain.Member) (*memberDomain.Member, error) {
	r.updates++
	return m, nil
}

func TestMarkLost_RejectsMemberWhileCoverageIsStillValid(t *testing.T) {
	today := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	gymID, memberID := uuid.New(), uuid.New()
	expiry := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	repo := &markLostRepo{record: &memRepo.MemberWithMembership{
		Member:            &memberDomain.Member{ID: memberID, GymID: gymID, Status: memberDomain.StatusActive},
		CurrentMembership: &membershipDomain.Membership{ExpiryDate: &expiry, Status: membershipDomain.StatusActive},
	}}
	uc := app.NewMarkLost(repo, fpFakeUoW{}, &fpFakeAudit{})
	uc.Now = func() time.Time { return today }
	_, err := uc.Execute(context.Background(), app.MarkLostInput{GymID: gymID, MemberID: memberID})
	if !errors.Is(err, memErrors.ErrMemberStillCovered) {
		t.Fatalf("err = %v, want ErrMemberStillCovered", err)
	}
	if repo.updates != 0 {
		t.Errorf("updates = %d, covered member must not mutate", repo.updates)
	}
}

func TestMarkLost_AllowsExpiredCoverage(t *testing.T) {
	today := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	gymID, memberID := uuid.New(), uuid.New()
	expiry := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	repo := &markLostRepo{record: &memRepo.MemberWithMembership{
		Member:            &memberDomain.Member{ID: memberID, GymID: gymID, Status: memberDomain.StatusActive},
		CurrentMembership: &membershipDomain.Membership{ExpiryDate: &expiry, Status: membershipDomain.StatusActive},
	}}
	uc := app.NewMarkLost(repo, fpFakeUoW{}, &fpFakeAudit{})
	uc.Now = func() time.Time { return today }
	out, err := uc.Execute(context.Background(), app.MarkLostInput{GymID: gymID, MemberID: memberID})
	if err != nil {
		t.Fatalf("mark expired lost: %v", err)
	}
	if out.Status != memberDomain.StatusLost || repo.updates != 1 {
		t.Errorf("status/updates = %s/%d, want lost/1", out.Status, repo.updates)
	}
}
