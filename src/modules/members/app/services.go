package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// MemberService is the cross-BC seam for `members`. Other BCs (billing in
// Sesión 3, checkins in Sesión 5) call methods here within their own
// UnitOfWork transactions.
type MemberService struct {
	Members         memRepo.MemberRepository
	Memberships     memRepo.MembershipRepository
	MembershipTypes memRepo.MembershipTypeRepository
}

func NewMemberService(members memRepo.MemberRepository, memberships memRepo.MembershipRepository,
	mtypes memRepo.MembershipTypeRepository) *MemberService {
	return &MemberService{Members: members, Memberships: memberships, MembershipTypes: mtypes}
}

// RenewMembershipForPaymentInput is the cross-BC input from billing/UC-018.
type RenewMembershipForPaymentInput struct {
	MemberID         uuid.UUID
	MembershipTypeID uuid.UUID
	PaymentDate      time.Time
}

type RenewMembershipForPaymentOutput struct {
	OldMembership *membershipDomain.Membership
	NewMembership *membershipDomain.Membership
	NextType      *mtDomain.MembershipType
}

// RenewMembershipForPayment runs the renewal logic of UC-018 (the half that
// lives in `members`): mark the old Membership replaced, create a new one
// with snapshot fields from the (possibly different) MembershipType, return
// both. Caller (billing) must be inside its own UnitOfWork.Command tx.
//
// NOTE: billing is wired in Sesión 3. This method is defined here so that
// the future implementation of UC-018 can stitch into a stable seam.
func (s *MemberService) RenewMembershipForPayment(ctx context.Context, tx sharedDomain.Transaction, in RenewMembershipForPaymentInput, now time.Time) (*RenewMembershipForPaymentOutput, error) {
	mt, err := s.MembershipTypes.GetByID(tx, in.MembershipTypeID)
	if err != nil {
		return nil, err
	}
	if !mt.Active {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeInactive, "")
	}
	current, err := s.Memberships.GetCurrentByMember(tx, in.MemberID)
	if err != nil {
		return nil, err
	}
	if current.GymID != mt.GymID {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
	}
	newID := uuid.New()
	next := current.Renew(newID, mt, in.PaymentDate, now)
	current.MarkReplaced(newID, now)
	if _, err := s.Memberships.Update(tx, current); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if _, err := s.Memberships.Create(tx, next); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return &RenewMembershipForPaymentOutput{
		OldMembership: current,
		NewMembership: next,
		NextType:      mt,
	}, nil
}

// GetAccessStatusInput is consumed by checkins/Sesión 5.
type GetAccessStatusInput struct {
	GymID    uuid.UUID
	MemberID uuid.UUID
	Today    time.Time // caller passes "today in gym timezone" — domain stays UTC-pure
}

type GetAccessStatusOutput struct {
	Member            *memberDomain.Member
	CurrentMembership *membershipDomain.Membership
	Status            access.AccessStatus
}

// GetAccessStatus is a read helper. Uses the existing transaction (so checkins
// can hold a single tx for the whole "scan -> evaluate -> insert checkin" flow).
func (s *MemberService) GetAccessStatus(ctx context.Context, tx sharedDomain.Transaction, in GetAccessStatusInput) (*GetAccessStatusOutput, error) {
	mw, err := s.Members.GetWithCurrentMembership(tx, in.GymID, in.MemberID)
	if err != nil {
		return nil, err
	}
	if mw.Member == nil {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMemberNotFound, "")
	}
	today := in.Today
	if today.IsZero() {
		now := time.Now().UTC()
		today = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	}
	st := access.New().Evaluate(mw.Member, mw.CurrentMembership, today)
	return &GetAccessStatusOutput{
		Member:            mw.Member,
		CurrentMembership: mw.CurrentMembership,
		Status:            st,
	}, nil
}

// Compile-time assertion: domain types are reachable from this package without
// being used elsewhere — a deliberate seam check.
var _ = memberDomain.StatusActive
