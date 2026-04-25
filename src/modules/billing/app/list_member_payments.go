package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ListMemberPaymentsInput backs UC-021. Filters mirror the UI: by concept and
// by date range. Pagination is required because long-running members may
// accumulate years of payments.
type ListMemberPaymentsInput struct {
	GymID         uuid.UUID
	MemberID      uuid.UUID
	ConceptFilter string
	From          *time.Time
	To            *time.Time
	Page          int
	PageSize      int
}

type ListMemberPaymentsOutput struct {
	Items    []*paymentDomain.Payment
	Total    int
	Page     int
	PageSize int
}

type ListMemberPayments struct {
	Payments billingRepo.PaymentRepository
	Members  memRepo.MemberRepository
	UoW      sharedDomain.UnitOfWork
}

func NewListMemberPayments(payments billingRepo.PaymentRepository, members memRepo.MemberRepository,
	uow sharedDomain.UnitOfWork) *ListMemberPayments {
	return &ListMemberPayments{Payments: payments, Members: members, UoW: uow}
}

func (uc *ListMemberPayments) Execute(ctx context.Context, in ListMemberPaymentsInput) (*ListMemberPaymentsOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	member, err := uc.Members.GetByID(tx, in.MemberID)
	if err != nil {
		return nil, err
	}
	if member.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
	}
	page := in.Page
	if page < 1 {
		page = 1
	}
	pageSize := in.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 25
	}
	rows, total, err := uc.Payments.ListByMember(tx, billingRepo.ListByMemberQuery{
		GymID:         in.GymID,
		MemberID:      in.MemberID,
		ConceptFilter: in.ConceptFilter,
		From:          in.From,
		To:            in.To,
		Page:          page,
		PageSize:      pageSize,
	})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return &ListMemberPaymentsOutput{
		Items:    rows,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}, nil
}
