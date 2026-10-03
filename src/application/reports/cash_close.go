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
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expenseRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

type CashCloseReportInput struct {
	GymID              uuid.UUID
	Date               time.Time
	DrawerID           *uuid.UUID
	HideAdministrative bool
}

// CashCloseReportOutput is the composed read model the controller marshals.
// Brings together the billing aggregate, expenses for the day, and the
// closed-event metadata if the operator already confirmed the cierre.
type CashCloseReportOutput struct {
	Timezone string
	Entries  []billingRepo.CashLedgerEntry
	Date     time.Time
	Totals   *billingRepo.CashCloseTotals
	Expenses []CashCloseExpenseEntry
	// ExpensesTotal is the sum of expenses explicitly paid from today's
	// register. Expenses paid from the gym fund or external money do not belong
	// to the daily drawer reconciliation.
	ExpensesTotal float64
	// ExpensesByMethod is kept on the wire for compatibility. Under the current
	// invariant, cash_register expenses always use the cash method.
	ExpensesByMethod map[string]float64
	// Deprecated: NetTotal mixes collections from every payment method with
	// expenses from the selected physical drawer. It is retained for one wire
	// compatibility window, but must not be presented as business result or
	// physical cash flow. Use report PeriodResult or Session fields instead.
	NetTotal float64
	// Closed is non-nil cuando ya existe un cash_close_events del día.
	Closed                 *ClosedCashEvent
	CashMovements          []CashMovementEntry
	CashInTotal            float64
	CashOutTotal           float64
	AdministrativeIncluded bool
	Session                *CashSessionView
	// Sessions is the ordered physical history for the day. Session remains a
	// compatibility alias to the latest `main` drawer session.
	Sessions              []*CashSessionView
	SelectedDrawerID      uuid.UUID
	Drawers               []CashDrawerView
	UncoveredCashActivity float64
	RequiresNewSession    bool
	SuggestedOpeningCash  *float64
}

type CashSessionView struct {
	CurrentExpectedCash     *float64
	FinishedAt              *time.Time
	ClosedByName            *string
	DiscrepancyReason       *string
	ID                      uuid.UUID
	DrawerID                uuid.UUID
	DrawerCode              string
	OperationalDate         time.Time
	Sequence                int
	Status                  string
	OpeningCash             float64
	OpeningCashKnown        bool
	ActivityCash            float64
	ExpectedCash            float64
	CountedCash             *float64
	Difference              *float64
	CashLeft                *float64
	WithdrawnCash           *float64
	WithdrawalDestination   *string
	OpenedAt                time.Time
	ClosedAt                *time.Time
	ReconciledAt            *time.Time
	StaleAt                 *time.Time
	WithdrawnAt             *time.Time
	IsStale                 bool
	AdjustedAfterWithdrawal bool
	IntegrityNote           *string
	CorrectionReason        *string
	UncoveredCashActivity   float64
	RequiresNewSession      bool
}

type CashMovementEntry struct {
	ID                   uuid.UUID
	CashDrawerID         uuid.UUID
	MovementType, Reason string
	Amount               float64
	OperatorID           uuid.UUID
	ClassificationStatus string
}

type CashDrawerView struct {
	ID           uuid.UUID
	Code         string
	Name         string
	Active       bool
	IsMain       bool
	ActivityCash float64
	HasActivity  bool
	SessionCount int
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
	ClosedAt              time.Time
	CalculatedCash        float64
	CurrentCalculatedCash float64
	CountedCash           float64
	Diff                  float64 // counted - calculated; 0 cuando no se contó
	IsOutdated            bool
	Reason                *string
	ClosedByName          *string
}

type CashCloseInput struct {
	ExpectedCash      *float64
	SessionID         uuid.UUID
	Finish            bool
	GymID             uuid.UUID
	ActorUserID       uuid.UUID
	Date              time.Time
	DrawerID          *uuid.UUID
	OpeningCash       *float64
	CountedCash       *float64
	DiscrepancyReason *string
	CorrectionReason  *string
	CashLeft          *float64
	Withdraw          bool
}

type CashCloseOutput struct {
	CashCloseID    uuid.UUID
	CalculatedCash float64
	CountedCash    *float64
	Discrepancy    *float64
	Session        *CashSessionView
}

type CashReopenInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	SessionID   uuid.UUID
	Date        time.Time
	DrawerID    *uuid.UUID
	Reason      *string
}

type CashReconcileInput struct {
	Finish            bool
	GymID             uuid.UUID
	ActorUserID       uuid.UUID
	ActorRole         string
	SessionID         uuid.UUID
	CountedCash       float64
	DiscrepancyReason *string
	CorrectionReason  *string
}

type CashWithdrawInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	SessionID   uuid.UUID
	CashLeft    float64
	Destination string
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

// CashClose is the UC-027 use case. Report() reads; Close() and Reopen() write.
// Reader provides the billing aggregate. Expenses lists gastos del día, Users
// resolves the closer's name for the "caja cerrada por…" banner. Events both
// persists the cierre (Close) and looks up an existing one (Report).
type CashClose struct {
	Reader        billingRepo.CashCloseReader
	Events        billingRepo.CashCloseEventRepository
	Expenses      expenseRepo.ExpenseRepository
	CashMovements expenseRepo.CashMovementRepository
	Drawers       billingRepo.CashDrawerRepository
	Users         userRepo.UserRepository
	UoW           sharedDomain.UnitOfWork
	Audit         audit.Recorder
	Subscribers   []CashCloseSubscriber
	// Gyms (opcional) → default del "día de la caja" en el día LOCAL del
	// gym cuando el caller no manda fecha (ver localToday).
	Gyms gymRepo.GymRepository
}

func NewCashClose(reader billingRepo.CashCloseReader, events billingRepo.CashCloseEventRepository,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CashClose {
	return &CashClose{Reader: reader, Events: events, UoW: uow, Audit: recorder}
}

// WithExpenses wires the expenses BC repository — once provided, Report()
// includes gastos del día. Kept as a fluent setter so existing test fixtures
// keep working with the zero value (no expenses surfaced).
// WithGyms cablea el repo de gyms para el default de fecha del reporte.
func (uc *CashClose) WithGyms(g gymRepo.GymRepository) *CashClose {
	uc.Gyms = g
	return uc
}

func (uc *CashClose) WithExpenses(repo expenseRepo.ExpenseRepository) *CashClose {
	uc.Expenses = repo
	return uc
}

func (uc *CashClose) WithCashMovements(repo expenseRepo.CashMovementRepository) *CashClose {
	uc.CashMovements = repo
	return uc
}

func (uc *CashClose) WithCashDrawers(repo billingRepo.CashDrawerRepository) *CashClose {
	uc.Drawers = repo
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

// Report runs the complete per-day aggregation from one read snapshot. This is
// important for a cash close: totals, physical activity and the saved session
// must never describe different instants just because a payment synced between
// two queries.
func (uc *CashClose) Report(ctx context.Context, in CashCloseReportInput) (*CashCloseReportOutput, error) {
	var out *CashCloseReportOutput
	err := sharedDomain.ReadSnapshot(ctx, uc.UoW, func(tx sharedDomain.Transaction) error {
		var err error
		out, err = uc.report(ctx, tx, in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (uc *CashClose) report(ctx context.Context, tx sharedDomain.Transaction, in CashCloseReportInput) (*CashCloseReportOutput, error) {
	// Default de fecha: el día LOCAL del gym. El desktop siempre manda la
	// fecha; este fallback cubre callers sin query param — con el default
	// UTC anterior, pedir "la caja de hoy" después de las 6 PM devolvía
	// la caja (vacía) de mañana.
	if in.Date.IsZero() {
		in.Date = localToday(tx, uc.Gyms, in.GymID, nowUTC())
	}
	selectedDrawerID := cashCloseDomain.DefaultDrawerID(in.GymID)
	if in.DrawerID != nil && *in.DrawerID != uuid.Nil {
		selectedDrawerID = *in.DrawerID
	}
	totals, err := uc.Reader.Aggregate(tx, billingRepo.CashCloseQuery{
		GymID: in.GymID, Date: in.Date, DrawerID: selectedDrawerID,
	})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	out := &CashCloseReportOutput{
		Date:             dayUTC(in.Date),
		Totals:           totals,
		Expenses:         []CashCloseExpenseEntry{},
		ExpensesByMethod: map[string]float64{},
		CashMovements:    []CashMovementEntry{},
		Sessions:         []*CashSessionView{},
		SelectedDrawerID: selectedDrawerID,
		Drawers:          []CashDrawerView{},
	}
	_, out.Timezone = localTodayAndTZ(tx, uc.Gyms, in.GymID, nowUTC())
	out.Entries, err = uc.Reader.CashEntries(tx, billingRepo.CashCloseQuery{GymID: in.GymID, Date: in.Date, DrawerID: selectedDrawerID})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	drawerViews := map[uuid.UUID]*CashDrawerView{}
	ensureDrawer := func(drawerID uuid.UUID) *CashDrawerView {
		if drawerID == uuid.Nil {
			drawerID = cashCloseDomain.DefaultDrawerID(in.GymID)
		}
		if drawerViews[drawerID] == nil {
			code := cashCloseDomain.DrawerCode(in.GymID, drawerID)
			name := code
			isMain := drawerID == cashCloseDomain.DefaultDrawerID(in.GymID)
			if isMain {
				name = cashCloseDomain.DefaultDrawerName
			}
			drawerViews[drawerID] = &CashDrawerView{ID: drawerID, Code: code, Name: name, Active: true, IsMain: isMain}
		}
		return drawerViews[drawerID]
	}
	ensureDrawer(cashCloseDomain.DefaultDrawerID(in.GymID))
	ensureDrawer(selectedDrawerID)
	if uc.Drawers != nil {
		catalog, err := uc.Drawers.ListByGym(tx, in.GymID, true)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		for _, drawer := range catalog {
			view := ensureDrawer(drawer.ID)
			view.Code, view.Name, view.Active, view.IsMain = drawer.Code, drawer.Name, drawer.Active, drawer.IsMain
		}
	}
	physicalDrawers, err := uc.Reader.CashDrawers(tx, in.GymID, dayUTC(in.Date))
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	for _, drawer := range physicalDrawers {
		view := ensureDrawer(drawer.DrawerID)
		view.ActivityCash = drawer.ActivityCash
		view.HasActivity = true
	}
	selectedMovementIDs := map[uuid.UUID]bool{}
	movementScopeKnown := uc.CashMovements != nil
	if movementScopeKnown {
		moves, err := uc.CashMovements.ListByDate(tx, in.GymID, dayUTC(in.Date))
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		for _, m := range moves {
			drawerID := m.CashDrawerID
			if drawerID == uuid.Nil {
				drawerID = in.GymID
			}
			if drawerID != selectedDrawerID {
				continue
			}
			selectedMovementIDs[m.ID] = true
			out.CashMovements = append(out.CashMovements, CashMovementEntry{ID: m.ID, CashDrawerID: drawerID, MovementType: m.MovementType, Reason: m.Reason, Amount: m.Amount, OperatorID: m.OperatorID, ClassificationStatus: m.ClassificationStatus})
			if m.MovementType == cashDomain.CashIn {
				out.CashInTotal += m.Amount
			} else {
				out.CashOutTotal += m.Amount
			}
		}
	}

	// Only expenses explicitly paid from today's register belong in a cash
	// close. Fund/external expenses affect Resultado del período, not this drawer.
	includeAdmin := !in.HideAdministrative
	out.AdministrativeIncluded = includeAdmin
	if includeAdmin && uc.Expenses != nil {
		gastos, err := uc.Expenses.ListByDate(tx, in.GymID, dayUTC(in.Date))
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		appendDrawerExpenses(out, gastos, selectedMovementIDs, movementScopeKnown)
	}

	// RefundTotal es NEGATIVO (refunds con amount negativo), así que SUMARLO
	// resta los reembolsos del neto. El bug anterior `- RefundTotal` los
	// SUMABA al neto (inflaba el "qué ganó hoy" al doble del reembolso).
	out.NetTotal = totals.GrandTotal + totals.RefundTotal - out.ExpensesTotal

	// sessions[] is scoped to the selected drawer. drawers[] below remains the
	// global catalog/summary; mixing another drawer's sessions into this detail
	// made UI previews compare a global count with a drawer-local close.
	events, err := uc.Events.ListByDate(tx, in.GymID, dayUTC(in.Date))
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	latestByDrawer := make(map[uuid.UUID]*cashCloseDomain.CashCloseEvent)
	for _, event := range events {
		if previous := latestByDrawer[event.DrawerID]; previous == nil || event.Sequence > previous.Sequence {
			latestByDrawer[event.DrawerID] = event
		}
	}
	now := nowUTC()
	for _, event := range events {
		ensureDrawer(event.DrawerID).SessionCount++
		if event.DrawerID != selectedDrawerID {
			continue
		}
		isLatest := latestByDrawer[event.DrawerID] == event
		currentExpected := event.CalculatedCash
		isOutdated := event.Status == cashCloseDomain.StatusStale
		dynamicPostWithdrawalAdjustment := false
		uncovered := 0.0
		requiresNew := false
		if finished := event.FinishedAt(); finished != nil {
			historical, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{
				GymID: in.GymID, DrawerID: event.DrawerID, OperationalDate: event.OperationalDate,
				OpenedAt: event.OpenedAt, AsOf: *finished,
			})
			if err != nil {
				return nil, sharedDomain.NewUnexpectedError(err)
			}
			currentExpected = event.OpeningCash + historical.Net
			dynamicPostWithdrawalAdjustment = !event.AdjustedAfterWithdrawal &&
				math.Abs(currentExpected-event.CalculatedCash) > 0.001
			if isLatest {
				late, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{
					GymID: in.GymID, DrawerID: event.DrawerID, OperationalDate: event.OperationalDate,
					OpenedAt: nextSessionOpenedAt(event), AsOf: now,
				})
				if err != nil {
					return nil, sharedDomain.NewUnexpectedError(err)
				}
				uncovered = late.Net
				requiresNew = !late.Watermark.IsZero()
			}
		} else if isLatest {
			activity, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{
				GymID: in.GymID, DrawerID: event.DrawerID, OperationalDate: event.OperationalDate,
				OpenedAt: event.OpenedAt, AsOf: now,
			})
			if err != nil {
				return nil, sharedDomain.NewUnexpectedError(err)
			}
			currentExpected = event.OpeningCash + activity.Net
			isOutdated = isOutdated || (event.Status != cashCloseDomain.StatusOpen &&
				(math.Abs(currentExpected-event.CalculatedCash) > 0.001 ||
					(event.ClosedAt != nil && activity.Watermark.After(*event.ClosedAt))))
		}
		status := event.EffectiveStatus(isOutdated)
		view := cashSessionView(event, status, isOutdated, currentExpected)
		if event.FinishedAt() != nil {
			view.ExpectedCash = event.CalculatedCash
			if math.Abs(currentExpected-event.CalculatedCash) > 0.001 {
				value := currentExpected
				view.CurrentExpectedCash = &value
			}
		}
		if event.ClosedAt != nil {
			closed, err := uc.closedCashEvent(tx, event, currentExpected, isOutdated)
			if err != nil {
				return nil, err
			}
			view.ClosedByName = closed.ClosedByName
		}
		if dynamicPostWithdrawalAdjustment {
			view.AdjustedAfterWithdrawal = true
			view.Difference = nil
			note := "Se corrigieron movimientos de este corte. El conteo original se conserva."
			view.IntegrityNote = &note
		}
		view.UncoveredCashActivity = uncovered
		view.RequiresNewSession = requiresNew
		out.Sessions = append(out.Sessions, view)

		if isLatest {
			out.Session = view
			out.UncoveredCashActivity = uncovered
			out.RequiresNewSession = requiresNew
			if event.Status != cashCloseDomain.StatusOpen {
				closed, err := uc.closedCashEvent(tx, event, currentExpected,
					isOutdated || dynamicPostWithdrawalAdjustment)
				if err != nil {
					return nil, err
				}
				out.Closed = closed
			}
		}
	}
	if out.Session == nil {
		_, timezone := localTodayAndTZ(tx, uc.Gyms, in.GymID, now)
		start, _ := tz.DayBounds(timezone, in.Date, in.Date)
		activity, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{
			GymID: in.GymID, DrawerID: selectedDrawerID, OperationalDate: in.Date,
			OpenedAt: start, AsOf: now,
		})
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		out.UncoveredCashActivity = activity.Net
		out.RequiresNewSession = !activity.Watermark.IsZero()
	}
	if out.Session == nil {
		previous, err := uc.Events.LatestBefore(tx, in.GymID, selectedDrawerID, dayUTC(in.Date))
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		if previous != nil && previous.FinishedAt() != nil {
			out.SuggestedOpeningCash = previous.CashLeft
		}
	} else if out.Session.FinishedAt != nil {
		out.SuggestedOpeningCash = out.Session.CashLeft
	}
	for _, drawer := range drawerViews {
		out.Drawers = append(out.Drawers, *drawer)
	}
	sort.Slice(out.Drawers, func(i, j int) bool {
		mainID := cashCloseDomain.DefaultDrawerID(in.GymID)
		if out.Drawers[i].ID == mainID {
			return true
		}
		if out.Drawers[j].ID == mainID {
			return false
		}
		return out.Drawers[i].Code < out.Drawers[j].Code
	})

	return out, nil
}

func (uc *CashClose) closedCashEvent(tx sharedDomain.Transaction, event *cashCloseDomain.CashCloseEvent,
	currentExpected float64, isOutdated bool) (*ClosedCashEvent, error) {
	closedAt := event.UpdatedAt
	if event.ClosedAt != nil {
		closedAt = *event.ClosedAt
	}
	closed := &ClosedCashEvent{ClosedAt: closedAt, CalculatedCash: event.CalculatedCash,
		CurrentCalculatedCash: currentExpected, IsOutdated: isOutdated}
	if event.CountedCash != nil && !isOutdated && !event.AdjustedAfterWithdrawal {
		closed.CountedCash = *event.CountedCash
		if diff := event.Difference(); diff != nil {
			closed.Diff = *diff
		}
	}
	if event.DiscrepancyReason != nil {
		reason := *event.DiscrepancyReason
		closed.Reason = &reason
	}
	if uc.Users != nil && event.ClosedBy != uuid.Nil {
		user, err := uc.Users.GetByID(tx, event.ClosedBy)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		if user != nil {
			name := user.FullName
			closed.ClosedByName = &name
		}
	}
	return closed, nil
}

func appendDrawerExpenses(out *CashCloseReportOutput, expenses []*expenseDomain.Expense,
	selectedMovementIDs map[uuid.UUID]bool, movementScopeKnown bool) {
	for _, e := range expenses {
		if e == nil || e.PaidFrom != expenseDomain.PaidFromCashRegister {
			continue
		}
		if movementScopeKnown && (e.CashMovementID == nil || !selectedMovementIDs[*e.CashMovementID]) {
			continue
		}
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

// Close persists the cierre event. The aggregate runs first (re-using the
// same tx) so the snapshot of "what we had" is correct at write time. The
// operator's counted_cash + discrepancy_reason are stored — DA-27.2 only
// audits the gap, never blocks. Cash expenses paid that day are subtracted
// from the calculated cash so "what should be in the drawer" reflects
// gastos en efectivo que ya salieron.
func (uc *CashClose) Close(ctx context.Context, in CashCloseInput) (*CashCloseOutput, error) {
	now := nowUTC()
	var out CashCloseOutput
	replayed := false
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		today, timezone, calendarErr := cashCommandLocalTodayAndTZ(tx, uc.Gyms, in.GymID, now)
		if calendarErr != nil {
			return sharedDomain.NewUnexpectedError(calendarErr)
		}
		if in.Date.IsZero() {
			in.Date = today
		}
		in.Date = dayUTC(in.Date)
		if err := validateCashOperationalDate(in.Date, today); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		drawerID := cashCloseDomain.DefaultDrawerID(in.GymID)
		if in.DrawerID != nil && *in.DrawerID != uuid.Nil {
			drawerID = *in.DrawerID
		}
		if uc.Drawers != nil {
			drawer, drawerErr := uc.Drawers.GetByID(tx, in.GymID, drawerID)
			if drawerErr != nil {
				return sharedDomain.NewUnexpectedError(drawerErr)
			}
			if drawer == nil {
				return sharedDomain.NewBusinessError(cashCloseDomain.ErrDrawerNotFound, "")
			}
			if !drawer.Active {
				return sharedDomain.NewBusinessError(cashCloseDomain.ErrDrawerInactive, "")
			}
		}
		sessions, err := uc.Events.ListByDate(tx, in.GymID, in.Date)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		var session *cashCloseDomain.CashCloseEvent
		for _, candidate := range sessions {
			if candidate.DrawerID == drawerID && (session == nil || candidate.Sequence > session.Sequence) {
				session = candidate
			}
		}

		if in.Finish {
			if in.SessionID == uuid.Nil || in.CountedCash == nil {
				return sharedDomain.NewValidationError(errors.New("abre la caja y registra el efectivo contado"))
			}
			target, err := uc.Events.GetByID(tx, in.GymID, in.SessionID)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if target == nil || target.DrawerID != drawerID || !sameDay(target.OperationalDate, in.Date) {
				return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseNotFound, "")
			}
			if target.FinishedAt() != nil {
				same := target.CountedCash != nil && math.Abs(*target.CountedCash-*in.CountedCash) < 0.001 && stringOr(target.DiscrepancyReason, "") == stringOr(in.DiscrepancyReason, "")
				if in.Withdraw {
					same = same && target.WithdrawnAt != nil && in.CashLeft != nil && target.CashLeft != nil && math.Abs(*target.CashLeft-*in.CashLeft) < 0.001
				} else {
					same = same && target.WithdrawnAt == nil
				}
				if !same {
					return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseConflict, "el corte ya está guardado; actualiza la caja")
				}
				replayed = true
				out = CashCloseOutput{CashCloseID: target.ID, CalculatedCash: target.CalculatedCash, CountedCash: target.CountedCash, Discrepancy: target.Difference(), Session: cashSessionView(target, target.Status, target.Status == cashCloseDomain.StatusStale, target.CalculatedCash)}
				return nil
			}
			if session == nil || target.ID != session.ID {
				return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseConflict, "actualiza la caja antes de hacer el corte")
			}
		}
		if session == nil || session.FinishedAt() != nil {
			sequence := 1
			opening := 0.0
			start, _ := tz.DayBounds(timezone, in.Date, in.Date)
			if session != nil {
				sequence = session.Sequence + 1
				if session.CashLeft != nil {
					opening = *session.CashLeft
				}
				if session.FinishedAt() != nil {
					start = nextSessionOpenedAt(session)
				}
			}
			if in.OpeningCash != nil {
				opening = *in.OpeningCash
			}
			session, err = cashCloseDomain.Open(in.GymID, drawerID, in.ActorUserID,
				in.Date, sequence, opening, start)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if _, err = uc.Events.Create(tx, session); err != nil {
				if errors.Is(err, billingErrors.ErrCashCloseAlreadyExists) {
					return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseAlreadyExists, "")
				}
				return sharedDomain.NewUnexpectedError(err)
			}
			if err := uc.recordSessionAudit(ctx, tx, session, in.ActorUserID, audit.ActionCreate,
				map[string]any{"status": session.Status, "opening_cash": session.OpeningCash}, now); err != nil {
				return err
			}
		}

		activity, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{
			GymID: in.GymID, DrawerID: drawerID, OperationalDate: in.Date,
			OpenedAt: session.OpenedAt, AsOf: now,
		})
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		if in.ExpectedCash != nil && math.Abs(session.OpeningCash+activity.Net-*in.ExpectedCash) > 0.001 {
			return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseConflict, "hubo movimientos mientras contabas; revisa el efectivo esperado y vuelve a confirmar")
		}
		persistUpdate := func(action string, changes map[string]any) error {
			if _, err := uc.Events.Update(tx, session); err != nil {
				if errors.Is(err, billingErrors.ErrCashCloseConflict) {
					return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseConflict, "")
				}
				return sharedDomain.NewUnexpectedError(err)
			}
			return uc.recordSessionAudit(ctx, tx, session, in.ActorUserID, action, changes, now)
		}

		switch session.Status {
		case cashCloseDomain.StatusOpen:
			if err := session.Close(activity.Net, in.ActorUserID, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if err := persistUpdate(audit.ActionUpdate, map[string]any{
				"status": session.Status, "activity_cash": session.ActivityCash,
				"expected_cash": session.CalculatedCash,
			}); err != nil {
				return err
			}
		case cashCloseDomain.StatusStale:
			reason := stringOr(in.CorrectionReason, "actividad física posterior al corte")
			if err := session.Refresh(activity.Net, reason, in.ActorUserID, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if err := persistUpdate(audit.ActionUpdate, map[string]any{
				"reason": reason, "status": session.Status,
				"activity_cash": session.ActivityCash, "expected_cash": session.CalculatedCash,
			}); err != nil {
				return err
			}
		case cashCloseDomain.StatusClosedUnverified, cashCloseDomain.StatusReconciled:
			late := math.Abs(activity.Net-session.ActivityCash) > 0.001 ||
				(session.ClosedAt != nil && activity.Watermark.After(*session.ClosedAt))
			if !late {
				return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseAlreadyExists, "")
			}
			reason := stringOr(in.CorrectionReason, "actividad física posterior al corte")
			if err := session.MarkStale(reason, now); err != nil {
				return sharedDomain.NewBusinessError(err, "")
			}
			if err := persistUpdate(audit.ActionUpdate, map[string]any{"status": session.Status, "reason": reason}); err != nil {
				return err
			}
			if err := session.Refresh(activity.Net, reason, in.ActorUserID, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if err := persistUpdate(audit.ActionUpdate, map[string]any{
				"status": session.Status, "activity_cash": session.ActivityCash,
				"expected_cash": session.CalculatedCash,
			}); err != nil {
				return err
			}
		default:
			return sharedDomain.NewBusinessError(cashCloseDomain.ErrInvalidSessionState, "")
		}

		if in.CountedCash != nil {
			if err := session.Reconcile(*in.CountedCash, in.DiscrepancyReason, in.ActorUserID, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if err := persistUpdate(audit.ActionUpdate, map[string]any{
				"status": session.Status, "counted_cash": session.CountedCash,
				"difference": session.Difference(), "reason": session.DiscrepancyReason,
			}); err != nil {
				return err
			}
		}
		if in.Withdraw {
			if in.CashLeft == nil {
				return sharedDomain.NewValidationError(cashCloseDomain.ErrCashLeftRequired)
			}
			if err := session.Withdraw(*in.CashLeft, cashCloseDomain.DestinationGymFund, in.ActorUserID, now); err != nil {
				return sharedDomain.NewBusinessError(err, "")
			}
			if err := persistUpdate(audit.ActionUpdate, map[string]any{
				"status": session.Status, "cash_left": session.CashLeft,
				"withdrawn_cash": session.WithdrawnCash, "destination": session.WithdrawalDestination,
			}); err != nil {
				return err
			}
			transfer := cashCloseDomain.NewTransfer(session)
			if _, err := uc.Events.CreateTransfer(tx, transfer); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if err := uc.recordTransferAudit(ctx, tx, transfer, in.ActorUserID, now); err != nil {
				return err
			}
		}

		if in.Finish && !in.Withdraw {
			if err := session.Finish(now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if err := persistUpdate(audit.ActionUpdate, map[string]any{"cash_left": session.CashLeft, "finished_at": session.FinishedAt()}); err != nil {
				return err
			}
		}

		out = CashCloseOutput{
			CashCloseID: session.ID, CalculatedCash: session.CalculatedCash,
			CountedCash: session.CountedCash,
		}
		out.Discrepancy = session.Difference()
		out.Session = cashSessionView(session, session.Status, session.Status == cashCloseDomain.StatusStale, session.CalculatedCash)
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
	if !replayed && len(uc.Subscribers) > 0 {
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

// Reopen is the legacy date-based alias for marking the current session stale.
// It never tombstones the row: the next Close refreshes the same natural
// session and the audit ledger preserves the before/after snapshots.
func (uc *CashClose) Reopen(ctx context.Context, in CashReopenInput) error {
	now := nowUTC()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		today, _, calendarErr := cashCommandLocalTodayAndTZ(tx, uc.Gyms, in.GymID, now)
		if calendarErr != nil {
			return sharedDomain.NewUnexpectedError(calendarErr)
		}
		if in.Date.IsZero() {
			in.Date = today
		}
		in.Date = dayUTC(in.Date)
		if in.SessionID == uuid.Nil {
			if err := validateCashOperationalDate(in.Date, today); err != nil {
				return sharedDomain.NewValidationError(err)
			}
		}
		var event *cashCloseDomain.CashCloseEvent
		var err error
		if in.SessionID != uuid.Nil {
			event, err = uc.Events.GetByID(tx, in.GymID, in.SessionID)
		} else {
			drawerID := cashCloseDomain.DefaultDrawerID(in.GymID)
			if in.DrawerID != nil && *in.DrawerID != uuid.Nil {
				drawerID = *in.DrawerID
			}
			sessions, listErr := uc.Events.ListByDate(tx, in.GymID, in.Date)
			err = listErr
			for _, candidate := range sessions {
				if candidate.DrawerID == drawerID && (event == nil || candidate.Sequence > event.Sequence) {
					event = candidate
				}
			}
		}
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if event == nil {
			return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseNotFound, "")
		}
		reason := strings.TrimSpace(stringOr(in.Reason, ""))
		if reason == "" {
			return sharedDomain.NewValidationError(cashCloseDomain.ErrCorrectionReason)
		}
		if event.FinishedAt() != nil {
			return sharedDomain.NewBusinessError(cashCloseDomain.ErrInvalidSessionState, "el efectivo ya fue retirado; registra una nueva sesión")
		}
		if err := event.MarkStale(reason, now); err != nil {
			return sharedDomain.NewBusinessError(err, "")
		}
		if _, err := uc.Events.Update(tx, event); err != nil {
			if errors.Is(err, billingErrors.ErrCashCloseConflict) {
				return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseConflict, "")
			}
			return sharedDomain.NewUnexpectedError(err)
		}
		return uc.recordSessionAudit(ctx, tx, event, in.ActorUserID, audit.ActionUpdate,
			map[string]any{"status": event.Status, "reason": reason}, now)
	})
}

func (uc *CashClose) Reconcile(ctx context.Context, in CashReconcileInput) (*CashSessionView, error) {
	now := nowUTC()
	var view *CashSessionView
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		session, err := uc.Events.GetByID(tx, in.GymID, in.SessionID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if session == nil {
			return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseNotFound, "")
		}
		if session.FinishedAt() != nil {
			if in.Finish && session.WithdrawnAt == nil && session.CountedCash != nil && *session.CountedCash == in.CountedCash && stringOr(session.DiscrepancyReason, "") == stringOr(in.DiscrepancyReason, "") {
				view = cashSessionView(session, session.Status, session.Status == cashCloseDomain.StatusStale, session.CalculatedCash)
				return nil
			}
			return sharedDomain.NewBusinessError(cashCloseDomain.ErrInvalidSessionState, "el retiro ya fue registrado y no puede modificarse; abre una nueva sesión")
		}
		activity, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{
			GymID: in.GymID, DrawerID: session.DrawerID, OperationalDate: session.OperationalDate,
			OpenedAt: session.OpenedAt, AsOf: now,
		})
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		outdated := session.Status == cashCloseDomain.StatusStale || math.Abs(activity.Net-session.ActivityCash) > 0.001 ||
			(session.ClosedAt != nil && activity.Watermark.After(*session.ClosedAt))
		correctingCertified := session.Status == cashCloseDomain.StatusReconciled && !outdated
		if correctingCertified && session.CountedCash != nil && math.Abs(*session.CountedCash-in.CountedCash) < 0.001 {
			if in.Finish && session.FinishedAt() == nil {
				if err := session.Finish(now); err != nil {
					return sharedDomain.NewValidationError(err)
				}
				if _, err := uc.Events.Update(tx, session); err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				if err := uc.recordSessionAudit(ctx, tx, session, in.ActorUserID, audit.ActionUpdate, map[string]any{"cash_left": session.CashLeft}, now); err != nil {
					return err
				}
			}
			view = cashSessionView(session, session.Status, false, session.CalculatedCash)
			return nil // idempotent retry of the same count
		}
		if correctingCertified {
			if in.ActorRole != "owner" {
				return sharedDomain.NewBusinessError(cashCloseDomain.ErrCorrectionOwner, "")
			}
			if strings.TrimSpace(stringOr(in.CorrectionReason, "")) == "" {
				return sharedDomain.NewValidationError(cashCloseDomain.ErrCorrectionReason)
			}
		}
		if outdated {
			reason := stringOr(in.CorrectionReason, "actividad física posterior al corte")
			if session.Status != cashCloseDomain.StatusStale {
				if err := session.MarkStale(reason, now); err != nil {
					return sharedDomain.NewBusinessError(err, "")
				}
				if _, err := uc.Events.Update(tx, session); err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				if err := uc.recordSessionAudit(ctx, tx, session, in.ActorUserID, audit.ActionUpdate,
					map[string]any{"status": session.Status, "reason": reason}, now); err != nil {
					return err
				}
			}
			if err := session.Refresh(activity.Net, reason, in.ActorUserID, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if _, err := uc.Events.Update(tx, session); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}
		previousCount := session.CountedCash
		var reconcileErr error
		if correctingCertified {
			reconcileErr = session.CorrectReconciliation(in.CountedCash, in.DiscrepancyReason,
				stringOr(in.CorrectionReason, ""), in.ActorUserID, now)
		} else {
			reconcileErr = session.Reconcile(in.CountedCash, in.DiscrepancyReason, in.ActorUserID, now)
		}
		if reconcileErr != nil {
			return sharedDomain.NewValidationError(reconcileErr)
		}
		if in.Finish {
			if err := session.Finish(now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
		}
		if _, err := uc.Events.Update(tx, session); err != nil {
			if errors.Is(err, billingErrors.ErrCashCloseConflict) {
				return sharedDomain.NewBusinessError(err, "")
			}
			return sharedDomain.NewUnexpectedError(err)
		}
		changes := map[string]any{"status": session.Status, "counted_cash": session.CountedCash,
			"difference": session.Difference()}
		if correctingCertified {
			changes["previous_counted_cash"] = previousCount
			changes["correction_reason"] = session.CorrectionReason
		}
		if err := uc.recordSessionAudit(ctx, tx, session, in.ActorUserID, audit.ActionUpdate,
			changes, now); err != nil {
			return err
		}
		view = cashSessionView(session, session.Status, false, session.CalculatedCash)
		return nil
	})
	return view, err
}

func (uc *CashClose) Withdraw(ctx context.Context, in CashWithdrawInput) (*CashSessionView, error) {
	now := nowUTC()
	var view *CashSessionView
	var staleErr error
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		session, err := uc.Events.GetByID(tx, in.GymID, in.SessionID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if session == nil {
			return sharedDomain.NewBusinessError(billingErrors.ErrCashCloseNotFound, "")
		}
		if session.Status == cashCloseDomain.StatusWithdrawn {
			if session.CashLeft != nil && *session.CashLeft == in.CashLeft && (in.Destination == "" || in.Destination == cashCloseDomain.DestinationGymFund) {
				view = cashSessionView(session, session.Status, false, session.CalculatedCash)
				return nil
			}
			return sharedDomain.NewBusinessError(cashCloseDomain.ErrInvalidSessionState, "el retiro ya fue registrado y no puede modificarse; abre una nueva sesión")
		}
		if session.FinishedAt() != nil {
			today, _, calendarErr := cashCommandLocalTodayAndTZ(tx, uc.Gyms, in.GymID, now)
			if calendarErr != nil {
				return sharedDomain.NewUnexpectedError(calendarErr)
			}
			periods, err := uc.Events.ListByDate(tx, in.GymID, session.OperationalDate)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			for _, other := range periods {
				if other.DrawerID == session.DrawerID && other.Sequence > session.Sequence {
					return sharedDomain.NewBusinessError(cashCloseDomain.ErrInvalidSessionState, "ya comenzó el siguiente periodo; haz un nuevo corte para retirar")
				}
			}
			late, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{GymID: in.GymID, DrawerID: session.DrawerID, OperationalDate: session.OperationalDate, OpenedAt: nextSessionOpenedAt(session), AsOf: now})
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if !sameDay(today, session.OperationalDate) || !late.Watermark.IsZero() {
				return sharedDomain.NewBusinessError(cashCloseDomain.ErrInvalidSessionState, "hay un nuevo periodo; haz otro corte antes de retirar")
			}
		}
		activity, err := uc.Reader.SessionActivity(tx, billingRepo.CashSessionActivityQuery{
			GymID: in.GymID, DrawerID: session.DrawerID, OperationalDate: session.OperationalDate,
			OpenedAt: session.OpenedAt, AsOf: now,
		})
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if session.Status == cashCloseDomain.StatusStale || math.Abs(activity.Net-session.ActivityCash) > 0.001 ||
			(session.ClosedAt != nil && activity.Watermark.After(*session.ClosedAt)) {
			if session.Status != cashCloseDomain.StatusStale {
				reason := "actividad física posterior al corte"
				_ = session.MarkStale(reason, now)
				if _, err := uc.Events.Update(tx, session); err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				if err := uc.recordSessionAudit(ctx, tx, session, in.ActorUserID, audit.ActionUpdate,
					map[string]any{"status": session.Status, "reason": reason}, now); err != nil {
					return err
				}
			}
			view = cashSessionView(session, cashCloseDomain.StatusStale, true, session.OpeningCash+activity.Net)
			staleErr = sharedDomain.NewBusinessError(cashCloseDomain.ErrInvalidSessionState, "vuelve a contar antes de retirar")
			return nil // Commit the stale transition before surfacing the rejection.
		}
		if err := session.Withdraw(in.CashLeft, in.Destination, in.ActorUserID, now); err != nil {
			return sharedDomain.NewBusinessError(err, "")
		}
		if _, err := uc.Events.Update(tx, session); err != nil {
			if errors.Is(err, billingErrors.ErrCashCloseConflict) {
				return sharedDomain.NewBusinessError(err, "")
			}
			return sharedDomain.NewUnexpectedError(err)
		}
		transfer := cashCloseDomain.NewTransfer(session)
		if _, err := uc.Events.CreateTransfer(tx, transfer); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if err := uc.recordSessionAudit(ctx, tx, session, in.ActorUserID, audit.ActionUpdate,
			map[string]any{"status": session.Status, "cash_left": session.CashLeft,
				"withdrawn_cash": session.WithdrawnCash, "destination": session.WithdrawalDestination}, now); err != nil {
			return err
		}
		if err := uc.recordTransferAudit(ctx, tx, transfer, in.ActorUserID, now); err != nil {
			return err
		}
		view = cashSessionView(session, session.Status, false, session.CalculatedCash)
		return nil
	})
	if err == nil && staleErr != nil {
		return view, staleErr
	}
	return view, err
}

// MarkRecordCorrection implements billingApp.CashSessionCorrectionMarker.
// `record_only` changes the historical record to match what physically
// happened; it does not move cash. Before withdrawal, the existing count must
// therefore be repeated against the corrected expectation. After withdrawal,
// the transfer remains historical truth and only its certification is marked
// incomplete—never rewritten as if different money had been handed over.
func (uc *CashClose) MarkRecordCorrection(ctx context.Context, tx sharedDomain.Transaction,
	in billingApp.CashSessionAdjustmentInput) error {
	drawerID := cashCloseDomain.DefaultDrawerID(in.GymID)
	if in.DrawerID != nil && *in.DrawerID != uuid.Nil {
		drawerID = *in.DrawerID
	}
	return uc.markHistoricalCashAdjustment(ctx, tx, historicalCashAdjustment{
		GymID: in.GymID, ActorUserID: in.ActorUserID,
		OperationalDate: in.OperationalDate, OriginalRecordedAt: in.OriginalRecordedAt,
		DrawerID: drawerID, Reason: in.Reason,
		DefaultReason: "corrección administrativa de venta",
	})
}

// MarkPhysicalCashAdjustment implements expensesApp.CashSessionAdjustmentMarker.
// Unlike a new late event, editing/deleting an existing movement cannot be
// detected from created_at. The use case therefore invalidates the exact
// certification that originally covered the movement inside the same command.
func (uc *CashClose) MarkPhysicalCashAdjustment(ctx context.Context, tx sharedDomain.Transaction,
	in expApp.CashSessionAdjustmentInput) error {
	drawerID := in.DrawerID
	if drawerID == uuid.Nil {
		drawerID = cashCloseDomain.DefaultDrawerID(in.GymID)
	}
	return uc.markHistoricalCashAdjustment(ctx, tx, historicalCashAdjustment{
		GymID: in.GymID, ActorUserID: in.ActorUserID,
		OperationalDate: in.OperationalDate, OriginalRecordedAt: in.OriginalRecordedAt,
		DrawerID: drawerID, Reason: in.Reason, RequireReason: true,
	})
}

type historicalCashAdjustment struct {
	GymID, ActorUserID                  uuid.UUID
	OperationalDate, OriginalRecordedAt time.Time
	DrawerID                            uuid.UUID
	Reason, DefaultReason               string
	RequireReason                       bool
}

func (uc *CashClose) markHistoricalCashAdjustment(ctx context.Context, tx sharedDomain.Transaction,
	in historicalCashAdjustment) error {
	sessions, err := uc.Events.ListByDate(tx, in.GymID, dayUTC(in.OperationalDate))
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	var target *cashCloseDomain.CashCloseEvent
	recordedAt := in.OriginalRecordedAt.UTC()
	for _, candidate := range sessions {
		if candidate.DrawerID != in.DrawerID {
			continue
		}
		if !recordedAt.IsZero() {
			if recordedAt.Before(candidate.OpenedAt) {
				continue
			}
			if end := candidate.FinishedAt(); end != nil && recordedAt.After(*end) {
				continue
			}
		}
		if target == nil || candidate.Sequence > target.Sequence {
			target = candidate
		}
	}
	if target == nil || target.Status == cashCloseDomain.StatusOpen || target.Status == cashCloseDomain.StatusStale {
		return nil
	}
	now := nowUTC()
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		if in.RequireReason {
			return sharedDomain.NewValidationError(cashCloseDomain.ErrCorrectionReason)
		}
		reason = in.DefaultReason
	}
	if target.Status == cashCloseDomain.StatusWithdrawn {
		if target.AdjustedAfterWithdrawal && target.IntegrityNote != nil && *target.IntegrityNote == reason {
			return nil
		}
		if err := target.FlagAdjustedAfterWithdrawal(reason, now); err != nil {
			return sharedDomain.NewBusinessError(err, "")
		}
	} else {
		if err := target.MarkStale(reason, now); err != nil {
			return sharedDomain.NewBusinessError(err, "")
		}
	}
	if _, err := uc.Events.Update(tx, target); err != nil {
		if errors.Is(err, billingErrors.ErrCashCloseConflict) {
			return sharedDomain.NewBusinessError(err, "")
		}
		return sharedDomain.NewUnexpectedError(err)
	}
	return uc.recordSessionAudit(ctx, tx, target, in.ActorUserID, audit.ActionUpdate,
		map[string]any{"status": target.Status, "record_correction": reason,
			"adjusted_after_withdrawal": target.AdjustedAfterWithdrawal}, now)
}

var _ billingApp.CashSessionCorrectionMarker = (*CashClose)(nil)
var _ expApp.CashSessionAdjustmentMarker = (*CashClose)(nil)

func cashSessionView(e *cashCloseDomain.CashCloseEvent, status string, stale bool, currentExpected float64) *CashSessionView {
	if e == nil {
		return nil
	}
	view := &CashSessionView{
		ID: e.ID, DrawerID: e.DrawerID, DrawerCode: e.DrawerCode,
		FinishedAt: e.FinishedAt(), DiscrepancyReason: e.DiscrepancyReason,
		OperationalDate: e.OperationalDate, Sequence: e.Sequence, Status: status,
		OpeningCash: e.OpeningCash, OpeningCashKnown: e.OpeningCashKnown,
		ActivityCash: e.ActivityCash, ExpectedCash: currentExpected,
		CountedCash: e.CountedCash, CashLeft: e.CashLeft, WithdrawnCash: e.WithdrawnCash,
		WithdrawalDestination: e.WithdrawalDestination, OpenedAt: e.OpenedAt,
		ClosedAt: e.ClosedAt, ReconciledAt: e.ReconciledAt, StaleAt: e.StaleAt,
		WithdrawnAt: e.WithdrawnAt, IsStale: stale,
		AdjustedAfterWithdrawal: e.AdjustedAfterWithdrawal, IntegrityNote: e.IntegrityNote,
		CorrectionReason: e.CorrectionReason,
	}
	if !stale && !e.AdjustedAfterWithdrawal {
		view.Difference = e.Difference()
	}
	return view
}

func (uc *CashClose) recordSessionAudit(ctx context.Context, tx sharedDomain.Transaction,
	e *cashCloseDomain.CashCloseEvent, actor uuid.UUID, action string, changes map[string]any, at time.Time) error {
	if err := uc.Audit.Record(ctx, tx, audit.Entry{GymID: e.GymID, EntityType: "cash_close_events",
		EntityID: e.ID, Action: action, ActorUserID: &actor, Changes: changes,
		IPAddress: audit.IPFromContext(ctx), UserAgent: audit.UAFromContext(ctx), At: at}); err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	return nil
}

func (uc *CashClose) recordTransferAudit(ctx context.Context, tx sharedDomain.Transaction,
	tr *cashCloseDomain.CashTransfer, actor uuid.UUID, at time.Time) error {
	if tr == nil {
		return sharedDomain.NewUnexpectedError(cashCloseDomain.ErrInvalidSessionState)
	}
	if err := uc.Audit.Record(ctx, tx, audit.Entry{GymID: tr.GymID, EntityType: "cash_transfers",
		EntityID: tr.ID, Action: audit.ActionCreate, ActorUserID: &actor,
		Changes:   map[string]any{"session_id": tr.SessionID, "amount": tr.Amount, "destination": tr.Destination},
		IPAddress: audit.IPFromContext(ctx), UserAgent: audit.UAFromContext(ctx), At: at}); err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	return nil
}

func stringOr(v *string, fallback string) string {
	if v != nil && strings.TrimSpace(*v) != "" {
		return strings.TrimSpace(*v)
	}
	return fallback
}

// SQLite stores event timestamps in epoch milliseconds. A new sequence starts
// one storage tick after the withdrawal so the two inclusive query windows do
// not count the same event twice.
func nextSessionOpenedAt(previous *cashCloseDomain.CashCloseEvent) time.Time {
	if previous != nil && previous.FinishedAt() != nil {
		return previous.FinishedAt().UTC().Truncate(time.Millisecond).Add(time.Millisecond)
	}
	return time.Time{}
}

func calculateDrawerCash(totals *billingRepo.CashCloseTotals, cashInTotal, cashOutTotal float64) float64 {
	if totals == nil {
		return cashInTotal - cashOutTotal
	}
	// RefundByMethod[cash] is negative, so adding it removes refunded cash.
	return totals.ByMethod[paymentDomain.MethodCash] +
		totals.RefundByMethod[paymentDomain.MethodCash] +
		cashInTotal - cashOutTotal
}

// dayUTC truncates a timestamp to its UTC calendar day (00:00:00). Both
// Report and Close compare close_date / expense_date at day granularity, so
// we normalize at the edge.
func dayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// validateCashOperationalDate impide certificar una caja que todavía no
// existe en el calendario operativo del gimnasio. El día solicitado y
// "today" ya son fechas sin hora; mantener esta regla aquí evita comparar
// contra el día UTC del servidor (que desde las 6 PM ya es mañana en CDMX).
// Los cortes atrasados siguen siendo válidos.
func validateCashOperationalDate(requested, today time.Time) error {
	requested = dayUTC(requested)
	today = dayUTC(today)
	if requested.After(today) {
		return billingErrors.ErrCashCloseFutureDate
	}
	return nil
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}

// Compile-time guard that the expense domain entity exposes the fields we
// reach for here. Using the pkg avoids unused-import errors when no expense
// repo is wired (tests).
var _ = expenseDomain.Expense{}
