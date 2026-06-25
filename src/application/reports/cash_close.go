// Package reports holds cross-context read models — queries that span
// multiple bounded contexts and don't naturally belong inside any one BC.
// CLAUDE.md identifies this directory as "queries read-only cross-context".
//
// UC-027 (Corte de caja diario) lives here because it composes:
//   - billing aggregate (payments + sales totals by method, concept, operator)
//   - expenses BC (gastos del día — antes faltaban del corte; el dueño
//     necesita ver "qué entró - qué salió" para confiar en el efectivo)
//   - billing.cashclose write (when the operator confirms the cierre)
//
// The use case has two entry points: Report() returns the read model so the
// UI can render the totals; Close() persists the operator-confirmed cierre.
package reports

import (
	"context"
	"time"

	"github.com/google/uuid"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expenseRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CashCloseReportInput struct {
	GymID uuid.UUID
	Date  time.Time
}

// CashCloseReportOutput is the composed read model the controller marshals.
// Brings together the billing aggregate, expenses for the day, and the
// closed-event metadata if the operator already confirmed the cierre.
type CashCloseReportOutput struct {
	Date     time.Time
	Totals   *billingRepo.CashCloseTotals
	Expenses []CashCloseExpenseEntry
	// ExpensesTotal is the sum across every entry in Expenses (any method).
	ExpensesTotal float64
	// ExpensesByMethod splits gastos by payment_method — used by the close
	// modal to compute "cash que debería quedar en el cajón" once egresos
	// se descuentan.
	ExpensesByMethod map[string]float64
	// NetTotal = GrandTotal − RefundTotal − ExpensesTotal. El dueño quiere
	// ver el "qué ganó hoy" directo en la caja del día.
	NetTotal float64
	// Closed is non-nil cuando ya existe un cash_close_events del día.
	Closed *ClosedCashEvent
}

// CashCloseExpenseEntry — una fila de la tabla "Gastos del día" dentro del
// corte. Espejo mínimo de Expense; no incluye created_by porque el corte
// usa OperatorTotal para la lista de operadores.
type CashCloseExpenseEntry struct {
	ID            uuid.UUID
	Category      string
	Description   *string
	Amount        float64
	PaymentMethod string
}

// ClosedCashEvent — snapshot del cierre persistido. Surface solo cuando el
// FE necesita renderear el banner "caja cerrada".
type ClosedCashEvent struct {
	ClosedAt     time.Time
	CountedCash  float64
	Diff         float64 // counted - calculated; 0 cuando no se contó
	Reason       *string
	ClosedByName *string
}

type CashCloseInput struct {
	GymID             uuid.UUID
	ActorUserID       uuid.UUID
	Date              time.Time
	CountedCash       *float64
	DiscrepancyReason *string
}

type CashCloseOutput struct {
	CashCloseID    uuid.UUID
	CalculatedCash float64
	CountedCash    *float64
	Discrepancy    *float64
}

// CashCloseSubscriber is the post-commit hook fired after a successful
// Close(). Today only the notifications BC subscribes (owner alert
// `cash_close_diff`); kept generic so other BCs can plug in later without
// reaching back into reports/. Errors from the subscriber are logged but
// not surfaced — the cierre row already committed and the operator
// should not see a 5xx for a downstream alert hiccup.
type CashCloseSubscriber interface {
	OnCashCloseClosed(ctx context.Context, evt CashCloseClosedEvent)
}

// CashCloseClosedEvent is the payload handed to subscribers. Discrepancy
// is nil when no count was recorded, zero when the count matched, non-zero
// when there's a gap — subscribers decide how to react.
type CashCloseClosedEvent struct {
	GymID          uuid.UUID
	ActorUserID    uuid.UUID
	CloseDate      time.Time
	CalculatedCash float64
	CountedCash    *float64
	Discrepancy    *float64
	ClosedAt       time.Time
}

// CashClose is the UC-027 use case. Two methods: Report() reads, Close() writes.
// Reader provides the billing aggregate. Expenses lists gastos del día, Users
// resolves the closer's name for the "caja cerrada por…" banner. Events both
// persists the cierre (Close) and looks up an existing one (Report).
type CashClose struct {
	Reader      billingRepo.CashCloseReader
	Events      billingRepo.CashCloseEventRepository
	Expenses    expenseRepo.ExpenseRepository
	Users       userRepo.UserRepository
	UoW         sharedDomain.UnitOfWork
	Audit       audit.Recorder
	Subscribers []CashCloseSubscriber
}

func NewCashClose(reader billingRepo.CashCloseReader, events billingRepo.CashCloseEventRepository,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CashClose {
	return &CashClose{Reader: reader, Events: events, UoW: uow, Audit: recorder}
}

// WithExpenses wires the expenses BC repository — once provided, Report()
// includes gastos del día. Kept as a fluent setter so existing test fixtures
// keep working with the zero value (no expenses surfaced).
func (uc *CashClose) WithExpenses(repo expenseRepo.ExpenseRepository) *CashClose {
	uc.Expenses = repo
	return uc
}

// WithUsers wires the users repo for resolving the closer's full_name.
// Optional — when nil, the FE renders the closed banner without the actor's
// name.
func (uc *CashClose) WithUsers(repo userRepo.UserRepository) *CashClose {
	uc.Users = repo
	return uc
}

// WithSubscriber appends a post-commit subscriber. Returns the receiver so
// main.go can fluently chain `.WithSubscriber(...)`.
func (uc *CashClose) WithSubscriber(s CashCloseSubscriber) *CashClose {
	uc.Subscribers = append(uc.Subscribers, s)
	return uc
}

// Report runs the per-day aggregation. Read-only — uses Query() not Command().
func (uc *CashClose) Report(ctx context.Context, in CashCloseReportInput) (*CashCloseReportOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	totals, err := uc.Reader.Aggregate(tx, billingRepo.CashCloseQuery{
		GymID: in.GymID, Date: in.Date,
	})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	out := &CashCloseReportOutput{
		Date:             dayUTC(in.Date),
		Totals:           totals,
		Expenses:         []CashCloseExpenseEntry{},
		ExpensesByMethod: map[string]float64{},
	}

	// Gastos del día. Si el repo no está cableado o falla, el corte sigue
	// rindiendo: el dueño verá la sección vacía pero los demás números OK.
	if uc.Expenses != nil {
		gastos, err := uc.Expenses.ListByDate(tx, in.GymID, dayUTC(in.Date))
		if err == nil {
			for _, e := range gastos {
				entry := CashCloseExpenseEntry{
					ID:            e.ID,
					Category:      e.Category,
					Amount:        e.Amount,
					PaymentMethod: e.PaymentMethod,
				}
				if e.Description != nil {
					s := *e.Description
					entry.Description = &s
				}
				out.Expenses = append(out.Expenses, entry)
				out.ExpensesTotal += e.Amount
				out.ExpensesByMethod[e.PaymentMethod] += e.Amount
			}
		}
	}

	// RefundTotal es NEGATIVO (refunds con amount negativo), así que SUMARLO
	// resta los reembolsos del neto. El bug anterior `- RefundTotal` los
	// SUMABA al neto (inflaba el "qué ganó hoy" al doble del reembolso).
	out.NetTotal = totals.GrandTotal + totals.RefundTotal - out.ExpensesTotal

	// Closed-event lookup. ListByGym ordena por close_date DESC; recorremos
	// hasta encontrar el de esta fecha. Para volúmenes de gyms (≤1 cierre
	// por día por gym) un escaneo de 30 entradas alcanza, y evita un nuevo
	// método de repo sólo para esto.
	if events, err := uc.Events.ListByGym(tx, in.GymID, 30); err == nil {
		target := dayUTC(in.Date)
		for _, ev := range events {
			if !sameDay(ev.CloseDate, target) {
				continue
			}
			closed := &ClosedCashEvent{ClosedAt: ev.UpdatedAt}
			if ev.CountedCash != nil {
				closed.CountedCash = *ev.CountedCash
				closed.Diff = *ev.CountedCash - ev.CalculatedCash
			}
			if ev.DiscrepancyReason != nil {
				s := *ev.DiscrepancyReason
				closed.Reason = &s
			}
			if uc.Users != nil && ev.ClosedBy != uuid.Nil {
				if u, err := uc.Users.GetByID(tx, ev.ClosedBy); err == nil && u != nil {
					name := u.FullName
					closed.ClosedByName = &name
				}
			}
			out.Closed = closed
			break
		}
	}

	return out, nil
}

// Close persists the cierre event. The aggregate runs first (re-using the
// same tx) so the snapshot of "what we had" is correct at write time. The
// operator's counted_cash + discrepancy_reason are stored — DA-27.2 only
// audits the gap, never blocks. Cash expenses paid that day are subtracted
// from the calculated cash so "what should be in the drawer" reflects
// gastos en efectivo que ya salieron.
func (uc *CashClose) Close(ctx context.Context, in CashCloseInput) (*CashCloseOutput, error) {
	now := time.Now().UTC()
	var out CashCloseOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		totals, err := uc.Reader.Aggregate(tx, billingRepo.CashCloseQuery{
			GymID: in.GymID, Date: in.Date,
		})
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// "Calculated cash" = cobros en efectivo del día (ByMethod excluye
		// refunds) − gastos pagados en efectivo − reembolsos pagados en
		// efectivo. Los reembolsos cash sacan dinero físico del cajón pero
		// estaban fuera de ByMethod y NO se restaban → el corte reportaba un
		// faltante fantasma y disparaba una falsa alerta al dueño.
		calculated := totals.ByMethod[paymentDomain.MethodCash]
		// RefundByMethod[cash] es negativo → sumarlo descuenta los refunds cash.
		calculated += totals.RefundByMethod[paymentDomain.MethodCash]
		if uc.Expenses != nil {
			if gastos, err := uc.Expenses.ListByDate(tx, in.GymID, dayUTC(in.Date)); err == nil {
				for _, g := range gastos {
					if g.PaymentMethod == paymentDomain.MethodCash {
						calculated -= g.Amount
					}
				}
			}
		}
		event := cashCloseDomain.New(uuid.New(), in.GymID, in.ActorUserID,
			in.Date, calculated, in.CountedCash, in.DiscrepancyReason, now)
		if _, err := uc.Events.Create(tx, event); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "cash_close_events",
			EntityID:    event.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"close_date":         event.CloseDate.Format("2006-01-02"),
				"calculated_cash":    event.CalculatedCash,
				"counted_cash":       event.CountedCash,
				"discrepancy_reason": event.DiscrepancyReason,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = CashCloseOutput{
			CashCloseID:    event.ID,
			CalculatedCash: event.CalculatedCash,
			CountedCash:    event.CountedCash,
		}
		if event.CountedCash != nil {
			d := *event.CountedCash - event.CalculatedCash
			out.Discrepancy = &d
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Post-commit fan-out. We pass the same data we just persisted so the
	// subscriber doesn't have to re-read the cierre row. Subscribers run
	// synchronously inside the same request — they're expected to do
	// minimal work (e.g. enqueue a notification row) and never perform
	// network IO here.
	if len(uc.Subscribers) > 0 {
		evt := CashCloseClosedEvent{
			GymID:          in.GymID,
			ActorUserID:    in.ActorUserID,
			CloseDate:      in.Date,
			CalculatedCash: out.CalculatedCash,
			CountedCash:    out.CountedCash,
			Discrepancy:    out.Discrepancy,
			ClosedAt:       now,
		}
		for _, s := range uc.Subscribers {
			s.OnCashCloseClosed(ctx, evt)
		}
	}
	return &out, nil
}

// dayUTC truncates a timestamp to its UTC calendar day (00:00:00). Both
// Report and Close compare close_date / expense_date at day granularity, so
// we normalize at the edge.
func dayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}

// Compile-time guard that the expense domain entity exposes the fields we
// reach for here. Using the pkg avoids unused-import errors when no expense
// repo is wired (tests).
var _ = expenseDomain.Expense{}
