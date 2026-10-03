package app

import (
	"context"
	"errors"
	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	recurring "github.com/cuadra/cuadra-core/src/modules/expenses/domain/recurring"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type RecurringTemplateInput struct {
	GymID, ActorUserID                       uuid.UUID
	Name                                     string
	PayeeName                                *string
	Category                                 string
	ExpectedAmount                           float64
	PaymentMethod, Classification, Frequency string
	StartsOn                                 time.Time
	EndsOn                                   *time.Time
	ExpectedVersion                          int
	IdempotencyKey                           string
}
type CreateRecurringExpenseTemplate struct {
	Templates expRepo.RecurringTemplateRepository
	UoW       shared.UnitOfWork
	Audit     audit.Recorder
}

func NewCreateRecurringExpenseTemplate(r expRepo.RecurringTemplateRepository, u shared.UnitOfWork, a audit.Recorder) *CreateRecurringExpenseTemplate {
	return &CreateRecurringExpenseTemplate{r, u, a}
}
func (uc *CreateRecurringExpenseTemplate) Execute(ctx context.Context, in RecurringTemplateInput) (*recurring.Template, error) {
	key := strings.TrimSpace(in.IdempotencyKey)
	if len(key) > 120 {
		return nil, shared.NewValidationError(expErrors.ErrIdempotencyKeyInvalid)
	}
	// HTTP clients are required to send an idempotency key. Keep the
	// application service compatible with trusted/local callers created before
	// that contract: an omitted key gets a one-shot key instead of collapsing
	// unrelated templates onto the same deterministic identifier.
	if key == "" {
		key = uuid.NewString()
	}
	now := time.Now().UTC()
	var out *recurring.Template
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		templateID := uuid.NewSHA1(in.GymID, []byte("recurring-template:"+key))
		t, err := recurring.NewTemplate(recurring.TemplateInput{ID: templateID, GymID: in.GymID, CreatedBy: in.ActorUserID, Name: in.Name, PayeeName: in.PayeeName, Category: in.Category, ExpectedAmount: in.ExpectedAmount, PaymentMethod: in.PaymentMethod, Classification: in.Classification, Frequency: in.Frequency, StartsOn: in.StartsOn, EndsOn: in.EndsOn, Now: now})
		if err != nil {
			return shared.NewValidationError(err)
		}
		existing, getErr := uc.Templates.GetByID(tx, in.GymID, templateID)
		if getErr == nil {
			if !sameRecurringTemplateDefinition(existing, t) {
				return shared.NewBusinessError(expErrors.ErrTemplateIdempotencyConflict, "")
			}
			out = existing
			return nil
		}
		if !errors.Is(getErr, expErrors.ErrTemplateNotFound) {
			return getErr
		}
		if _, err = uc.Templates.Create(tx, t); err != nil {
			return shared.NewUnexpectedError(err)
		}
		if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "recurring_expense_templates", t.ID, audit.ActionCreate, map[string]any{"name": t.Name, "frequency": t.Frequency, "next_due_on": t.NextDueOn.Format("2006-01-02"), "idempotency_key": key}, now); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

type UpdateRecurringExpenseTemplate struct {
	Templates   expRepo.RecurringTemplateRepository
	Occurrences expRepo.OccurrenceRepository
	UoW         shared.UnitOfWork
	Audit       audit.Recorder
	Policy      OperationalPolicy
}

func NewUpdateRecurringExpenseTemplate(r expRepo.RecurringTemplateRepository, o expRepo.OccurrenceRepository, u shared.UnitOfWork, a audit.Recorder, p OperationalPolicy) *UpdateRecurringExpenseTemplate {
	return &UpdateRecurringExpenseTemplate{r, o, u, a, p}
}
func (uc *UpdateRecurringExpenseTemplate) Execute(ctx context.Context, id uuid.UUID, in RecurringTemplateInput) (*recurring.Template, error) {
	now := time.Now().UTC()
	var out *recurring.Template
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		t, err := uc.Templates.GetByID(tx, in.GymID, id)
		if err != nil {
			return err
		}
		if in.ExpectedVersion < 1 || in.ExpectedVersion != t.Version {
			return shared.NewBusinessError(expErrors.ErrVersionConflict, "")
		}
		v := t.Version
		candidate := *t
		if err = candidate.Update(recurring.TemplateInput{Name: in.Name, PayeeName: in.PayeeName, Category: in.Category, ExpectedAmount: in.ExpectedAmount, PaymentMethod: in.PaymentMethod, Classification: in.Classification, Frequency: in.Frequency, StartsOn: in.StartsOn, EndsOn: in.EndsOn}, now); err != nil {
			return shared.NewValidationError(err)
		}
		scheduleChanged := t.Frequency != candidate.Frequency || !dateOnly(t.StartsOn).Equal(dateOnly(candidate.StartsOn)) || !sameOptionalDate(t.EndsOn, candidate.EndsOn)
		if scheduleChanged {
			hasPending, pendingErr := uc.Occurrences.HasPendingByTemplate(tx, in.GymID, t.ID)
			if pendingErr != nil {
				return shared.NewUnexpectedError(pendingErr)
			}
			if hasPending {
				return shared.NewBusinessError(expErrors.ErrTemplateScheduleHasPending, "")
			}
		}
		today, err := uc.Policy.localToday(tx, in.GymID, now)
		if err != nil {
			return err
		}
		if scheduleChanged {
			candidate.NextDueOn, err = recurring.FirstDueOnOrAfter(candidate.StartsOn, candidate.Frequency, today)
			if err != nil {
				return shared.NewValidationError(err)
			}
		} else {
			candidate.NextDueOn = t.NextDueOn
		}
		*t = candidate
		if _, err = uc.Templates.Update(tx, t, v); err != nil {
			return shared.NewUnexpectedError(err)
		}
		if err = uc.Occurrences.UpdatePendingSnapshots(tx, t, today); err != nil {
			return shared.NewUnexpectedError(err)
		}
		if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "recurring_expense_templates", t.ID, audit.ActionUpdate, map[string]any{"name": t.Name, "expected_amount": t.ExpectedAmount, "schedule_changed": scheduleChanged, "next_due_on": t.NextDueOn.Format("2006-01-02")}, now); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

type DeactivateRecurringExpenseTemplate struct {
	Templates expRepo.RecurringTemplateRepository
	UoW       shared.UnitOfWork
	Audit     audit.Recorder
	Policy    OperationalPolicy
}

func NewDeactivateRecurringExpenseTemplate(r expRepo.RecurringTemplateRepository, u shared.UnitOfWork, a audit.Recorder) *DeactivateRecurringExpenseTemplate {
	return &DeactivateRecurringExpenseTemplate{Templates: r, UoW: u, Audit: a}
}

func (uc *DeactivateRecurringExpenseTemplate) WithOperationalPolicy(policy OperationalPolicy) *DeactivateRecurringExpenseTemplate {
	uc.Policy = policy
	return uc
}

func (uc *DeactivateRecurringExpenseTemplate) Execute(ctx context.Context, gym, actor, id uuid.UUID, reactivate bool, expectedVersion int) (*recurring.Template, error) {
	now := time.Now().UTC()
	var out *recurring.Template
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		t, err := uc.Templates.GetByID(tx, gym, id)
		if err != nil {
			return err
		}
		if t.Active == reactivate {
			out = t
			return nil
		}
		if expectedVersion < 1 || expectedVersion != t.Version {
			return shared.NewBusinessError(expErrors.ErrVersionConflict, "")
		}
		v := t.Version
		if reactivate {
			today, todayErr := uc.Policy.localToday(tx, gym, now)
			if todayErr != nil {
				return todayErr
			}
			resumeDue, dueErr := recurring.FirstDueOnOrAfter(t.StartsOn, t.Frequency, today)
			if dueErr != nil {
				return shared.NewValidationError(dueErr)
			}
			// Keep a farther NextDueOn already advanced by materialization. This
			// avoids revisiting dates that were generated before the pause while
			// still skipping every ungenerated date that elapsed while paused.
			if t.NextDueOn.After(resumeDue) {
				resumeDue = t.NextDueOn
			}
			t.Reactivate(now)
			t.NextDueOn = resumeDue
		} else {
			t.Deactivate(now)
		}
		if _, err = uc.Templates.Update(tx, t, v); err != nil {
			return shared.NewUnexpectedError(err)
		}
		if err = recordFinancial(ctx, uc.Audit, tx, gym, actor, "recurring_expense_templates", id, "set_active", map[string]any{
			"active": t.Active, "next_due_on": t.NextDueOn.Format("2006-01-02"),
			"semantics": "pause_stops_future_generation_existing_occurrences_remain",
		}, now); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

type ListRecurringExpenseTemplates struct {
	Templates expRepo.RecurringTemplateRepository
	UoW       shared.UnitOfWork
}

func NewListRecurringExpenseTemplates(r expRepo.RecurringTemplateRepository, u shared.UnitOfWork) *ListRecurringExpenseTemplates {
	return &ListRecurringExpenseTemplates{r, u}
}
func (uc *ListRecurringExpenseTemplates) Execute(ctx context.Context, gym uuid.UUID, include bool) ([]*recurring.Template, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, shared.NewUnexpectedError(err)
	}
	x, err := uc.Templates.List(tx, gym, include)
	if err != nil {
		return nil, shared.NewUnexpectedError(err)
	}
	return x, nil
}

type MaterializeExpenseOccurrences struct {
	Templates   expRepo.RecurringTemplateRepository
	Occurrences expRepo.OccurrenceRepository
	UoW         shared.UnitOfWork
	Audit       audit.Recorder
}

func NewMaterializeExpenseOccurrences(t expRepo.RecurringTemplateRepository, o expRepo.OccurrenceRepository, u shared.UnitOfWork, a audit.Recorder) *MaterializeExpenseOccurrences {
	return &MaterializeExpenseOccurrences{t, o, u, a}
}
func (uc *MaterializeExpenseOccurrences) Execute(ctx context.Context, gym uuid.UUID, to time.Time) (int, error) {
	now := time.Now().UTC()
	if to.IsZero() || to.After(dateOnly(now).AddDate(0, 0, 400)) {
		return 0, shared.NewValidationErrorWithMessage(expErrors.ErrInvalidDate, "la fecha no puede estar a más de 400 días en el futuro")
	}
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return 0, err
	}
	templates, err := uc.Templates.List(tx, gym, false)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, template := range templates {
		// Catch up in bounded transactions. An old cursor must neither discard
		// unpaid dates nor prevent current templates from being processed.
		retries := 0
		for {
			created, more, err := uc.materializeTemplateBatch(ctx, gym, template.ID, to, now)
			if errors.Is(err, expErrors.ErrVersionConflict) && retries < 3 {
				retries++
				continue
			}
			if err != nil {
				return count, err
			}
			retries = 0
			count += created
			if !more {
				break
			}
		}
	}
	return count, nil
}

func (uc *MaterializeExpenseOccurrences) materializeTemplateBatch(ctx context.Context, gym, id uuid.UUID, to, now time.Time) (int, bool, error) {
	count, more := 0, false
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		t, err := uc.Templates.GetByID(tx, gym, id)
		if err != nil {
			return err
		}
		if !t.Active || t.NextDueOn.After(to) || (t.EndsOn != nil && t.NextDueOn.After(*t.EndsOn)) {
			return nil
		}
		due := t.NextDueOn
		for visited := 0; !due.After(to) && visited < 400; visited++ {
			if t.EndsOn != nil && due.After(*t.EndsOn) {
				break
			}
			o := recurring.NewOccurrence(t, due, now)
			created, err := uc.Occurrences.CreateIfAbsent(tx, o)
			if err != nil {
				return shared.NewUnexpectedError(err)
			}
			if created {
				count++
				if err = uc.Audit.Record(ctx, tx, audit.Entry{GymID: gym, EntityType: "expense_occurrences", EntityID: o.ID, Action: audit.ActionCreate, Changes: map[string]any{"template_id": o.TemplateID, "due_on": o.DueOn.Format("2006-01-02"), "expected_amount": o.ExpectedAmount}, At: now}); err != nil {
					return err
				}
			}
			due, err = recurring.NextDue(due, t.Frequency, t.StartsOn.Day())
			if err != nil {
				return shared.NewValidationError(err)
			}
		}
		if !due.Equal(t.NextDueOn) {
			v := t.Version
			t.NextDueOn, t.UpdatedAt = due, now
			t.Version++
			if _, err = uc.Templates.Update(tx, t, v); err != nil {
				return shared.NewUnexpectedError(err)
			}
		}
		more = !due.After(to) && (t.EndsOn == nil || !due.After(*t.EndsOn))
		return nil
	})
	return count, more, err
}

type ListExpenseOccurrences struct {
	Templates   expRepo.RecurringTemplateRepository
	Expenses    expRepo.ExpenseRepository
	Occurrences expRepo.OccurrenceRepository
	UoW         shared.UnitOfWork
}

func NewListExpenseOccurrences(o expRepo.OccurrenceRepository, u shared.UnitOfWork) *ListExpenseOccurrences {
	return &ListExpenseOccurrences{Occurrences: o, UoW: u}
}

func (uc *ListExpenseOccurrences) WithDetails(t expRepo.RecurringTemplateRepository, e expRepo.ExpenseRepository) *ListExpenseOccurrences {
	uc.Templates, uc.Expenses = t, e
	return uc
}
func (uc *ListExpenseOccurrences) hydrate(tx shared.Transaction, items []*recurring.Occurrence) error {
	names := map[uuid.UUID]string{}
	for _, o := range items {
		if uc.Templates != nil {
			name, ok := names[o.TemplateID]
			if !ok {
				template, err := uc.Templates.GetByID(tx, o.GymID, o.TemplateID)
				if err != nil {
					return err
				}
				name = template.Name
				names[o.TemplateID] = name
			}
			o.Name = name
		}
		if uc.Expenses != nil && o.Status == recurring.Paid && o.ExpenseID != nil {
			expense, err := uc.Expenses.GetByID(tx, o.GymID, *o.ExpenseID)
			if err != nil {
				return err
			}
			amount, day := expense.Amount, expense.PaidOn
			o.PaidAmount, o.PaidOn = &amount, &day
		}
	}
	return nil
}

func validateOccurrenceQuery(q expRepo.OccurrenceQuery) error {
	if !q.From.IsZero() && !q.To.IsZero() && (q.From.After(q.To) || q.To.Sub(q.From) > 400*24*time.Hour) {
		return shared.NewValidationError(expErrors.ErrInvalidDate)
	}
	if q.Status != "" && q.Status != recurring.Pending && q.Status != recurring.Paid && q.Status != recurring.Skipped {
		return shared.NewValidationError(expErrors.ErrInvalidOccurrenceStatus)
	}
	return nil
}

func (uc *ListExpenseOccurrences) Execute(ctx context.Context, q expRepo.OccurrenceQuery) ([]*recurring.Occurrence, error) {
	if err := validateOccurrenceQuery(q); err != nil {
		return nil, err
	}
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, shared.NewUnexpectedError(err)
	}
	x, err := uc.Occurrences.List(tx, q)
	if err != nil {
		return nil, shared.NewUnexpectedError(err)
	}
	if err := uc.hydrate(tx, x); err != nil {
		return nil, err
	}
	return x, nil
}

type ExpenseOccurrencePage struct {
	Items    []*recurring.Occurrence
	Total    int
	Page     int
	PageSize int
}

func (uc *ListExpenseOccurrences) ExecutePage(ctx context.Context, q expRepo.OccurrenceQuery, page, pageSize int) (ExpenseOccurrencePage, error) {
	if page < 1 || pageSize < 1 || pageSize > 200 {
		return ExpenseOccurrencePage{}, shared.NewValidationError(expErrors.ErrInvalidPagination)
	}
	if err := validateOccurrenceQuery(q); err != nil {
		return ExpenseOccurrencePage{}, err
	}
	q.Limit = pageSize
	q.Offset = (page - 1) * pageSize
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return ExpenseOccurrencePage{}, shared.NewUnexpectedError(err)
	}
	items, err := uc.Occurrences.List(tx, q)
	if err != nil {
		return ExpenseOccurrencePage{}, shared.NewUnexpectedError(err)
	}
	total, err := uc.Occurrences.Count(tx, q)
	if err != nil {
		return ExpenseOccurrencePage{}, shared.NewUnexpectedError(err)
	}
	if err := uc.hydrate(tx, items); err != nil {
		return ExpenseOccurrencePage{}, err
	}
	return ExpenseOccurrencePage{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

type MarkOccurrencePaidInput struct {
	CashMovementID                    *uuid.UUID
	ExistingExpenseID                 *uuid.UUID
	GymID, ActorUserID, OccurrenceID  uuid.UUID
	PaidOn                            time.Time
	Amount                            float64
	PaymentMethod                     string
	PaidFrom                          string
	PayeeName, Description, Reference *string
	CashDrawerID                      *uuid.UUID
	IdempotencyKey                    string
}
type MarkExpenseOccurrencePaid struct {
	Occurrences   expRepo.OccurrenceRepository
	Expenses      expRepo.ExpenseRepository
	Movements     expRepo.CashMovementRepository
	UoW           shared.UnitOfWork
	Audit         audit.Recorder
	Policy        OperationalPolicy
	DrawerCatalog CashDrawerValidator
}

func NewMarkExpenseOccurrencePaid(o expRepo.OccurrenceRepository, e expRepo.ExpenseRepository, m expRepo.CashMovementRepository, u shared.UnitOfWork, a audit.Recorder, p OperationalPolicy) *MarkExpenseOccurrencePaid {
	return &MarkExpenseOccurrencePaid{Occurrences: o, Expenses: e, Movements: m, UoW: u, Audit: a, Policy: p}
}

func (uc *MarkExpenseOccurrencePaid) WithCashDrawerValidator(validator CashDrawerValidator) *MarkExpenseOccurrencePaid {
	uc.DrawerCatalog = validator
	return uc
}

func (uc *MarkExpenseOccurrencePaid) Execute(ctx context.Context, in MarkOccurrencePaidInput) (*expenseDomain.Expense, error) {
	if expenseDomain.NormalizePaidFrom(in.PaidFrom) == "" {
		return nil, shared.NewValidationError(expErrors.ErrInvalidPaidFrom)
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if len(key) > 120 {
		return nil, shared.NewValidationError(expErrors.ErrIdempotencyKeyInvalid)
	}
	now := time.Now().UTC()
	var out *expenseDomain.Expense
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		o, err := uc.Occurrences.GetByID(tx, in.GymID, in.OccurrenceID)
		if err != nil {
			return err
		}
		if in.ExistingExpenseID != nil {
			e, err := uc.Expenses.GetByID(tx, in.GymID, *in.ExistingExpenseID)
			if err != nil {
				return err
			}
			if o.Status == recurring.Paid && o.ExpenseID != nil && *o.ExpenseID == e.ID {
				out = e
				return nil
			}
			if o.Status != recurring.Pending || e.RecurringOccurrenceID != nil ||
				e.Category != o.Category || (e.Source != expenseDomain.SourceManual && e.Source != expenseDomain.SourceCashMovement) ||
				(in.CashMovementID != nil) || mathRoundCents(e.Amount) != mathRoundCents(in.Amount) ||
				!dateOnly(e.PaidOn).Equal(dateOnly(in.PaidOn)) || e.PaymentMethod != in.PaymentMethod ||
				expenseDomain.NormalizePaidFrom(e.PaidFrom) != expenseDomain.NormalizePaidFrom(in.PaidFrom) {
				return shared.NewBusinessError(expErrors.ErrLinkedExpense, "El gasto seleccionado ya cambió o no corresponde a este pago. Revisa los datos.")
			}
			if e.CashMovementID != nil {
				if uc.Movements == nil {
					return shared.NewUnexpectedError(errors.New("cash repository missing"))
				}
				movement, err := uc.Movements.GetByID(tx, in.GymID, *e.CashMovementID)
				if err != nil {
					return err
				}
				if movement.ClassificationStatus != cashDomain.AsExpense || movement.ExpenseID == nil || *movement.ExpenseID != e.ID {
					return shared.NewBusinessError(expErrors.ErrLinkedExpense, "Revisa la salida de caja del gasto seleccionado.")
				}
			}

			if err := e.SetMetadata(e.PayeeName, e.Reference, e.Classification, expenseDomain.SourceRecurring, &o.ID, e.CashMovementID); err != nil {
				return shared.NewValidationError(err)
			}
			e.Version++
			e.UpdatedAt = now
			if _, err := uc.Expenses.Update(tx, e); err != nil {
				return err
			}
			ov := o.Version
			if err := o.MarkPaid(e.ID, in.ActorUserID, now); err != nil {
				return err
			}
			if _, err := uc.Occurrences.Update(tx, o, ov); err != nil {
				return err
			}
			if err := recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expenses", e.ID, "relate_scheduled_payment", map[string]any{"recurring_occurrence_id": o.ID}, now); err != nil {
				return err
			}
			if err := recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expense_occurrences", o.ID, "mark_paid", map[string]any{"expense_id": e.ID, "existing_expense": true}, now); err != nil {
				return err
			}
			out = e
			return nil
		}
		paid := in.PaidOn
		if paid.IsZero() {
			paid = o.DueOn
		}
		amount := in.Amount
		if amount == 0 {
			amount = o.ExpectedAmount
		}
		method := in.PaymentMethod
		if method == "" {
			method = o.PaymentMethod
		}
		payee := in.PayeeName
		if payee == nil {
			payee = o.PayeeName
		}
		paidFrom := expenseDomain.NormalizePaidFrom(in.PaidFrom)
		if in.CashMovementID != nil && paidFrom != expenseDomain.PaidFromCashRegister {
			return shared.NewValidationError(expErrors.ErrInvalidPaidFrom)
		}
		drawerID := effectiveCashDrawerID(in.GymID, in.CashDrawerID)
		generation := strconv.Itoa(o.Version)
		expenseID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("occurrence-expense:"+o.ID.String()+":"+generation))
		var candidateMovementID *uuid.UUID
		if paidFrom == expenseDomain.PaidFromCashRegister {
			id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("occurrence-cash:"+o.ID.String()+":"+generation))
			candidateMovementID = &id
			if in.CashMovementID != nil {
				candidateMovementID = in.CashMovementID
			}
		}
		candidate, err := expenseDomain.NewPaid(expenseDomain.PaidInput{ID: expenseID, GymID: in.GymID, CreatedBy: in.ActorUserID, PaidOn: paid, Amount: amount, Category: o.Category, PaymentMethod: method, PaidFrom: paidFrom, PayeeName: payee, Description: in.Description, Reference: in.Reference, Classification: o.Classification, Source: expenseDomain.SourceRecurring, RecurringOccurrenceID: &o.ID, CashMovementID: candidateMovementID, Now: now})
		if err != nil {
			return shared.NewValidationError(err)
		}
		if err = validateDrawerAppliesToExpense(candidate, in.CashDrawerID); err != nil {
			return err
		}
		if o.Status == recurring.Paid && o.ExpenseID != nil {
			stored, getErr := uc.Expenses.GetByID(tx, in.GymID, *o.ExpenseID)
			if getErr != nil {
				return getErr
			}
			same := sameExpensePayload(stored, candidate)
			if same {
				same, getErr = expenseUsesDrawer(tx, uc.Movements, stored, drawerID)
			}
			if getErr != nil {
				return shared.NewUnexpectedError(getErr)
			}
			if !same {
				return shared.NewBusinessError(expErrors.ErrIdempotencyKeyConflict, "")
			}
			out = stored
			return nil
		}
		if o.Status != recurring.Pending {
			return shared.NewBusinessError(recurring.ErrResolved, "")
		}
		if err = uc.Policy.validatePaidDate(tx, in.GymID, paid, now); err != nil {
			return err
		}
		var movement *cashDomain.CashMovement
		if paidFrom == expenseDomain.PaidFromCashRegister {
			if uc.Movements == nil {
				return shared.NewUnexpectedError(errors.New("cash movement repository is not configured"))
			}
			if err = validateRequestedCashDrawer(tx, uc.DrawerCatalog, in.GymID, drawerID); err != nil {
				return err
			}
			movementID := *candidateMovementID
			reason := "Pago recurrente"
			if payee != nil {
				reason = *payee
			}
			if in.CashMovementID != nil {
				movement, err = uc.Movements.GetByID(tx, in.GymID, *in.CashMovementID)
				if err != nil {
					return err
				}
				if movement.MovementType != cashDomain.CashOut || movement.ClassificationStatus != cashDomain.Unclassified || movement.ExpenseID != nil ||
					!dateOnly(movement.MovementOn).Equal(dateOnly(paid)) || mathRoundCents(movement.Amount) != mathRoundCents(amount) || effectiveCashDrawerID(in.GymID, &movement.CashDrawerID) != drawerID {
					return shared.NewBusinessError(expErrors.ErrCashMovementNotCompatible, "")
				}
			} else {
				movement, err = cashDomain.New(movementID, in.GymID, in.ActorUserID, paid, amount, cashDomain.CashOut, reason, now)
			}
			if err != nil {
				return shared.NewValidationError(err)
			}
			movement.WithCashDrawer(drawerID)
			if in.CashMovementID == nil {
				if _, err = uc.Movements.Create(tx, movement); err != nil {
					return shared.NewUnexpectedError(err)
				}
			}
		}
		var movementID *uuid.UUID
		if movement != nil {
			movementID = &movement.ID
		}
		e := candidate
		e.CashMovementID = movementID
		if movement != nil {
			d := movement.CashDrawerID
			e.CashDrawerID = &d
		}
		if _, err = uc.Expenses.Create(tx, e); err != nil {
			return shared.NewUnexpectedError(err)
		}
		if movement != nil {
			v := movement.Version
			if err = movement.Classify(&e.ID, cashDomain.AsExpense, now); err != nil {
				return shared.NewValidationError(err)
			}
			if _, err = uc.Movements.Update(tx, movement, v); err != nil {
				return shared.NewUnexpectedError(err)
			}
		}
		ov := o.Version
		if err = o.MarkPaid(e.ID, in.ActorUserID, now); err != nil {
			return shared.NewBusinessError(err, "")
		}
		if _, err = uc.Occurrences.Update(tx, o, ov); err != nil {
			return shared.NewUnexpectedError(err)
		}
		if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expenses", e.ID, audit.ActionCreate, map[string]any{"recurring_occurrence_id": o.ID, "amount": e.Amount, "payment_method": e.PaymentMethod, "cash_drawer_id": func() any {
			if movement == nil {
				return nil
			}
			return movement.CashDrawerID
		}(), "idempotency_key": key}, now); err != nil {
			return err
		}
		if movement != nil {
			action := audit.ActionCreate
			if in.CashMovementID != nil {
				action = audit.ActionUpdate
			}
			if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "cash_movements", movement.ID, action, map[string]any{"expense_id": e.ID, "amount": movement.Amount, "classification_status": movement.ClassificationStatus, "cash_drawer_id": movement.CashDrawerID}, now); err != nil {
				return err
			}
		}
		if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expense_occurrences", o.ID, "mark_paid", map[string]any{"expense_id": e.ID, "amount": e.Amount, "payment_method": e.PaymentMethod, "idempotency_key": key}, now); err != nil {
			return err
		}
		out = e
		return nil
	})
	return out, err
}

type SkipExpenseOccurrence struct {
	Occurrences expRepo.OccurrenceRepository
	UoW         shared.UnitOfWork
	Audit       audit.Recorder
}

func NewSkipExpenseOccurrence(o expRepo.OccurrenceRepository, u shared.UnitOfWork, a audit.Recorder) *SkipExpenseOccurrence {
	return &SkipExpenseOccurrence{o, u, a}
}
func (uc *SkipExpenseOccurrence) Execute(ctx context.Context, gym, actor, id uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > 200 {
		return shared.NewValidationError(expErrors.ErrInvalidDescription)
	}
	now := time.Now().UTC()
	return uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		o, err := uc.Occurrences.GetByID(tx, gym, id)
		if err != nil {
			return err
		}
		if o.Status == recurring.Skipped {
			return nil
		}
		v := o.Version
		if err = o.Skip(actor, reason, now); err != nil {
			return shared.NewBusinessError(err, "")
		}
		if _, err = uc.Occurrences.Update(tx, o, v); err != nil {
			return shared.NewUnexpectedError(err)
		}
		return recordFinancial(ctx, uc.Audit, tx, gym, actor, "expense_occurrences", id, "skip", map[string]any{"reason": reason}, now)
	})
}

func sameRecurringTemplateDefinition(a, b *recurring.Template) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.GymID == b.GymID && a.Name == b.Name && sameOptionalString(a.PayeeName, b.PayeeName) &&
		a.Category == b.Category && mathRoundCents(a.ExpectedAmount) == mathRoundCents(b.ExpectedAmount) &&
		a.PaymentMethod == b.PaymentMethod && a.Classification == b.Classification && a.Frequency == b.Frequency &&
		dateOnly(a.StartsOn).Equal(dateOnly(b.StartsOn)) && sameOptionalDate(a.EndsOn, b.EndsOn)
}

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameOptionalDate(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return dateOnly(*a).Equal(dateOnly(*b))
}

func mathRoundCents(v float64) int64 {
	return int64(v*100 + 0.5)
}
