package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ListGymPaymentsInput backs the gym-wide cobranza screen — same filter
// surface as ListMemberPayments but unbounded to a single member so the
// operator sees every payment recorded today/this-period.
type ListGymPaymentsInput struct {
	GymID         uuid.UUID
	ConceptFilter string
	From          time.Time
	To            time.Time
	Page          int
	PageSize      int
}

type ListGymPaymentsOutput struct {
	Items         []*paymentDomain.Payment
	Total         int
	Page          int
	PageSize      int
	TotalPaid     float64 // sum of non-refund amounts in the filtered window
	CashTotal     float64 // breakdown by method (excludes refunds + soft-deleted)
	TransferTotal float64
	CardTotal     float64
	MemberNames   map[uuid.UUID]string // id → full_name for the rows in this page
}

type ListGymPayments struct {
	Payments billingRepo.PaymentRepository
	Members  memRepo.MemberRepository
	UoW      sharedDomain.UnitOfWork
}

func NewListGymPayments(payments billingRepo.PaymentRepository, members memRepo.MemberRepository, uow sharedDomain.UnitOfWork) *ListGymPayments {
	return &ListGymPayments{Payments: payments, Members: members, UoW: uow}
}

func (uc *ListGymPayments) Execute(ctx context.Context, in ListGymPaymentsInput) (*ListGymPaymentsOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	page := in.Page
	if page < 1 {
		page = 1
	}
	pageSize := in.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	from := in.From
	if from.IsZero() {
		// Default: last 30 days.
		from = time.Now().UTC().AddDate(0, 0, -30)
	}
	to := in.To
	if to.IsZero() {
		to = time.Now().UTC()
	}
	rows, total, err := uc.Payments.ListByGymBetweenDates(tx, billingRepo.ListByGymQuery{
		GymID:         in.GymID,
		From:          from,
		To:            to,
		ConceptFilter: in.ConceptFilter,
		Page:          page,
		PageSize:      pageSize,
	})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	var totalPaid, cashTotal, transferTotal, cardTotal float64
	memberIDs := make([]uuid.UUID, 0, len(rows))
	seen := make(map[uuid.UUID]struct{}, len(rows))
	for _, p := range rows {
		if p.MemberID != nil {
			if _, ok := seen[*p.MemberID]; !ok {
				seen[*p.MemberID] = struct{}{}
				memberIDs = append(memberIDs, *p.MemberID)
			}
		}
		if p.DeletedAt != nil || p.Concept == "refund" {
			continue
		}
		totalPaid += p.Amount
		switch p.PaymentMethod {
		case "cash":
			cashTotal += p.Amount
		case "transfer":
			transferTotal += p.Amount
		case "card":
			cardTotal += p.Amount
		}
	}
	names, err := uc.Members.GetNamesByIDs(tx, memberIDs)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return &ListGymPaymentsOutput{
		Items:         rows,
		Total:         total,
		Page:          page,
		PageSize:      pageSize,
		TotalPaid:     totalPaid,
		CashTotal:     cashTotal,
		TransferTotal: transferTotal,
		CardTotal:     cardTotal,
		MemberNames:   names,
	}, nil
}
