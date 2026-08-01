package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ListGymPaymentsInput backs the gym-wide cobranza screen — same filter
// surface as ListMemberPayments but unbounded to a single member so the
// operator sees every payment recorded today/this-period.
type ListGymPaymentsInput struct {
	GymID         uuid.UUID
	ConceptFilter string
	MethodFilter  string
	From          time.Time
	To            time.Time
	Page          int
	PageSize      int
}

// ListGymPaymentsOutput — items paginados + agregados de la ventana COMPLETA
// (no de la página): sumar sólo las filas visibles sub-reportaba el total en
// cuanto había >1 página y el número cambiaba al paginar. TotalPaid es NETO
// (los refunds viven con monto negativo y restan solos); RefundTotal trae la
// magnitud devuelta para mostrarse aparte. Los por-método son netos del
// método — mismo criterio que el corte de caja (un refund cash saca efectivo
// del cajón).
type ListGymPaymentsOutput struct {
	Items         []*paymentDomain.Payment
	Total         int
	Page          int
	PageSize      int
	TotalPaid     float64
	RefundTotal   float64
	CashTotal     float64
	TransferTotal float64
	CardTotal     float64
	MemberNames   map[uuid.UUID]string // id → full_name for the rows in this page
}

type ListGymPayments struct {
	Payments billingRepo.PaymentRepository
	Members  memRepo.MemberRepository
	UoW      sharedDomain.UnitOfWork
	// Gyms (opcional) → default de rango en el día LOCAL del gym cuando el
	// caller no manda from/to (ver gymLocalPaymentDate). Nil = día UTC.
	Gyms gymRepo.GymRepository
}

func NewListGymPayments(payments billingRepo.PaymentRepository, members memRepo.MemberRepository, uow sharedDomain.UnitOfWork) *ListGymPayments {
	return &ListGymPayments{Payments: payments, Members: members, UoW: uow}
}

// WithGyms cablea el repo de gyms para anclar el rango default al día
// calendario del gym en SU zona horaria.
func (uc *ListGymPayments) WithGyms(g gymRepo.GymRepository) *ListGymPayments {
	uc.Gyms = g
	return uc
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
	// Default de rango: "hoy" = día LOCAL del gym. El default UTC anterior
	// pedía el día de MAÑANA desde las 6 PM de CDMX y la pantalla de cobros
	// amanecía vacía cada noche.
	from, to := in.From, in.To
	if from.IsZero() || to.IsZero() {
		today := gymLocalPaymentDate(tx, uc.Gyms, in.GymID, time.Now().UTC())
		if to.IsZero() {
			to = today
		}
		if from.IsZero() {
			from = to
		}
	}
	q := billingRepo.ListByGymQuery{
		GymID:         in.GymID,
		From:          from,
		To:            to,
		ConceptFilter: in.ConceptFilter,
		MethodFilter:  in.MethodFilter,
		Page:          page,
		PageSize:      pageSize,
	}
	rows, total, err := uc.Payments.ListByGymBetweenDates(tx, q)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	agg, err := uc.Payments.AggregateByGymBetweenDates(tx, q)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	memberIDs := make([]uuid.UUID, 0, len(rows))
	seen := make(map[uuid.UUID]struct{}, len(rows))
	for _, p := range rows {
		if p.MemberID != nil {
			if _, ok := seen[*p.MemberID]; !ok {
				seen[*p.MemberID] = struct{}{}
				memberIDs = append(memberIDs, *p.MemberID)
			}
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
		TotalPaid:     agg.NetTotal,
		RefundTotal:   agg.RefundTotal,
		CashTotal:     agg.CashTotal,
		TransferTotal: agg.TransferTotal,
		CardTotal:     agg.CardTotal,
		MemberNames:   names,
	}, nil
}
