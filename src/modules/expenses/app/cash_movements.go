package app

import (
	"context"
	"errors"
	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type CashMovementInput struct {
	GymID, ActorUserID   uuid.UUID
	MovementOn           time.Time
	Amount               float64
	MovementType, Reason string
	CashDrawerID         *uuid.UUID
	IdempotencyKey       string
	CorrectionReason     string
	ExpectedVersion      int
	// Empty purpose preserves unclassified outflows from older clients.
	Purpose, Category string
}
type CreateCashMovement struct {
	Repo          expRepo.CashMovementRepository
	UoW           shared.UnitOfWork
	Audit         audit.Recorder
	Policy        OperationalPolicy
	DrawerCatalog CashDrawerValidator
	Expenses      expRepo.ExpenseRepository
}

func NewCreateCashMovement(r expRepo.CashMovementRepository, u shared.UnitOfWork, a audit.Recorder, p OperationalPolicy) *CreateCashMovement {
	return &CreateCashMovement{Repo: r, UoW: u, Audit: a, Policy: p}
}
func (uc *CreateCashMovement) WithCashDrawerValidator(validator CashDrawerValidator) *CreateCashMovement {
	uc.DrawerCatalog = validator
	return uc
}
func (uc *CreateCashMovement) WithExpenses(expenses expRepo.ExpenseRepository) *CreateCashMovement {
	uc.Expenses = expenses
	return uc
}
func (uc *CreateCashMovement) Execute(ctx context.Context, in CashMovementInput) (*cashDomain.CashMovement, error) {
	if (in.Purpose != "" && in.Purpose != cashDomain.AsExpense && in.Purpose != cashDomain.NonOperating) ||
		(in.Purpose != "" && in.MovementType != cashDomain.CashOut) ||
		(in.Category != "" && in.Purpose != cashDomain.AsExpense) {
		return nil, shared.NewValidationError(expErrors.ErrInvalidCashMovementPurpose)
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if len(key) > 120 {
		return nil, shared.NewValidationError(expErrors.ErrIdempotencyKeyInvalid)
	}
	now := time.Now().UTC()
	var out *cashDomain.CashMovement
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := uc.Policy.validatePaidDate(tx, in.GymID, in.MovementOn, now); err != nil {
			return err
		}
		drawerID := effectiveCashDrawerID(in.GymID, in.CashDrawerID)
		movementID := uuid.New()
		if key != "" {
			movementID = uuid.NewSHA1(in.GymID, []byte("cash-movement:"+key))
			stored, lookupErr := uc.Repo.GetByID(tx, in.GymID, movementID)
			if lookupErr == nil {
				if !dateOnly(stored.MovementOn).Equal(dateOnly(in.MovementOn)) ||
					math.Round(stored.Amount*100) != math.Round(in.Amount*100) ||
					stored.MovementType != in.MovementType || stored.Reason != strings.TrimSpace(in.Reason) ||
					stored.CashDrawerID != drawerID {
					return shared.NewBusinessError(expErrors.ErrIdempotencyKeyConflict, "")
				}
				if in.Purpose != "" && stored.ClassificationStatus != in.Purpose {
					return shared.NewBusinessError(expErrors.ErrIdempotencyKeyConflict, "")
				}
				if in.Purpose == cashDomain.AsExpense {
					if stored.ExpenseID == nil || uc.Expenses == nil {
						return shared.NewBusinessError(expErrors.ErrIdempotencyKeyConflict, "")
					}
					e, err := uc.Expenses.GetByID(tx, in.GymID, *stored.ExpenseID)
					if err != nil {
						return err
					}
					if e.Category != in.Category {
						return shared.NewBusinessError(expErrors.ErrIdempotencyKeyConflict, "")
					}
				}
				out = stored
				return nil
			}
			if !errors.Is(lookupErr, expErrors.ErrCashMovementNotFound) {
				return shared.NewUnexpectedError(lookupErr)
			}
		}
		if err := validateRequestedCashDrawer(tx, uc.DrawerCatalog, in.GymID, drawerID); err != nil {
			return err
		}
		m, err := cashDomain.New(movementID, in.GymID, in.ActorUserID, in.MovementOn, in.Amount, in.MovementType, in.Reason, now)
		if err != nil {
			return shared.NewValidationError(err)
		}
		m.WithCashDrawer(drawerID)
		if _, err = uc.Repo.Create(tx, m); err != nil {
			return shared.NewUnexpectedError(err)
		}
		if in.Purpose != "" {
			var expenseID *uuid.UUID
			if in.Purpose == cashDomain.AsExpense {
				if uc.Expenses == nil {
					return shared.NewUnexpectedError(errors.New("expense repository is not configured"))
				}
				e, newErr := expenseDomain.NewPaid(expenseDomain.PaidInput{
					ID: uuid.NewSHA1(m.ID, []byte("cash-expense:v1")), GymID: in.GymID,
					CreatedBy: in.ActorUserID, PaidOn: m.MovementOn, Amount: m.Amount,
					Category: in.Category, Description: &m.Reason,
					PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashRegister,
					Classification: "variable", Source: expenseDomain.SourceCashMovement,
					CashMovementID: &m.ID, Now: now,
				})
				if newErr != nil {
					return shared.NewValidationError(newErr)
				}
				if _, err = uc.Expenses.Create(tx, e); err != nil {
					return shared.NewUnexpectedError(err)
				}
				expenseID = &e.ID
				if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expenses", e.ID, audit.ActionCreate,
					map[string]any{"cash_movement_id": m.ID, "cash_drawer_id": drawerID, "amount": e.Amount, "category": e.Category}, now); err != nil {
					return err
				}
			}
			version := m.Version
			if err = m.Classify(expenseID, in.Purpose, now); err != nil {
				return shared.NewValidationError(err)
			}
			if _, err = uc.Repo.Update(tx, m, version); err != nil {
				return shared.NewUnexpectedError(err)
			}
		}
		if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "cash_movements", m.ID, audit.ActionCreate, map[string]any{"movement_on": m.MovementOn.Format("2006-01-02"), "amount": m.Amount, "movement_type": m.MovementType, "reason": m.Reason, "classification_status": m.ClassificationStatus, "idempotency_key": key}, now); err != nil {
			return err
		}
		out = m
		return nil
	})
	return out, err
}

type UpdateCashMovement struct {
	Repo          expRepo.CashMovementRepository
	UoW           shared.UnitOfWork
	Audit         audit.Recorder
	Policy        OperationalPolicy
	Marker        CashSessionAdjustmentMarker
	DrawerCatalog CashDrawerValidator
}

func NewUpdateCashMovement(r expRepo.CashMovementRepository, u shared.UnitOfWork, a audit.Recorder, p OperationalPolicy) *UpdateCashMovement {
	return &UpdateCashMovement{Repo: r, UoW: u, Audit: a, Policy: p}
}
func (uc *UpdateCashMovement) WithCashSessionMarker(marker CashSessionAdjustmentMarker) *UpdateCashMovement {
	uc.Marker = marker
	return uc
}
func (uc *UpdateCashMovement) WithCashDrawerValidator(validator CashDrawerValidator) *UpdateCashMovement {
	uc.DrawerCatalog = validator
	return uc
}
func (uc *UpdateCashMovement) Execute(ctx context.Context, id uuid.UUID, in CashMovementInput) (*cashDomain.CashMovement, error) {
	correctionReason := strings.TrimSpace(in.CorrectionReason)
	if utf8.RuneCountInString(correctionReason) < 3 || utf8.RuneCountInString(correctionReason) > 200 {
		return nil, shared.NewValidationError(expErrors.ErrCorrectionReasonRequired)
	}
	in.CorrectionReason = correctionReason
	now := time.Now().UTC()
	var out *cashDomain.CashMovement
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		m, err := uc.Repo.GetByID(tx, in.GymID, id)
		if err != nil {
			return err
		}
		if !m.CanCorrectPhysical() {
			return shared.NewBusinessError(expErrors.ErrLinkedCashMovement, "")
		}
		if in.ExpectedVersion > 0 && in.ExpectedVersion != m.Version {
			return shared.NewBusinessError(expErrors.ErrVersionConflict, "")
		}
		if err = uc.Policy.validatePaidDate(tx, in.GymID, in.MovementOn, now); err != nil {
			return err
		}
		originalDate := m.MovementOn
		originalRecordedAt := m.CreatedAt
		originalDrawerID := m.CashDrawerID
		if originalDrawerID == uuid.Nil {
			originalDrawerID = m.GymID
		}
		destinationDrawerID := originalDrawerID
		if in.CashDrawerID != nil && *in.CashDrawerID != uuid.Nil {
			destinationDrawerID = *in.CashDrawerID
			if err = validateRequestedCashDrawer(tx, uc.DrawerCatalog, in.GymID, destinationDrawerID); err != nil {
				return err
			}
		}
		physicalChanged := !dateOnly(m.MovementOn).Equal(dateOnly(in.MovementOn)) ||
			m.Amount != in.Amount || m.MovementType != in.MovementType || originalDrawerID != destinationDrawerID
		v := m.Version
		if err = m.Update(in.MovementOn, in.Amount, in.MovementType, in.Reason, now); err != nil {
			return shared.NewValidationError(err)
		}
		m.WithCashDrawer(destinationDrawerID)
		if _, err = uc.Repo.Update(tx, m, v); err != nil {
			return shared.NewUnexpectedError(err)
		}
		if physicalChanged && uc.Marker != nil {
			if err = uc.Marker.MarkPhysicalCashAdjustment(ctx, tx, CashSessionAdjustmentInput{
				GymID: in.GymID, ActorUserID: in.ActorUserID,
				OperationalDate: originalDate, OriginalRecordedAt: originalRecordedAt,
				DrawerID: originalDrawerID, Reason: in.CorrectionReason,
			}); err != nil {
				return err
			}
			// Moving a record into another day/drawer also invalidates any
			// certification already present at the destination. A zero record
			// timestamp deliberately selects its latest session: the edit is an
			// administrative placement, not a newly-created physical event.
			if !dateOnly(originalDate).Equal(dateOnly(m.MovementOn)) || originalDrawerID != m.CashDrawerID {
				if err = uc.Marker.MarkPhysicalCashAdjustment(ctx, tx, CashSessionAdjustmentInput{
					GymID: in.GymID, ActorUserID: in.ActorUserID,
					OperationalDate: m.MovementOn, DrawerID: m.CashDrawerID,
					Reason: in.CorrectionReason,
				}); err != nil {
					return err
				}
			}
		}
		if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "cash_movements", m.ID, audit.ActionUpdate, map[string]any{"amount": m.Amount, "movement_type": m.MovementType}, now); err != nil {
			return err
		}
		out = m
		return nil
	})
	return out, err
}

type DeleteCashMovement struct {
	Repo   expRepo.CashMovementRepository
	UoW    shared.UnitOfWork
	Audit  audit.Recorder
	Policy OperationalPolicy
	Marker CashSessionAdjustmentMarker
}

type DeleteCashMovementOptions struct {
	ExpectedVersion  int
	CorrectionReason string
}

func NewDeleteCashMovement(r expRepo.CashMovementRepository, u shared.UnitOfWork, a audit.Recorder, p OperationalPolicy) *DeleteCashMovement {
	return &DeleteCashMovement{Repo: r, UoW: u, Audit: a, Policy: p}
}
func (uc *DeleteCashMovement) WithCashSessionMarker(marker CashSessionAdjustmentMarker) *DeleteCashMovement {
	uc.Marker = marker
	return uc
}
func (uc *DeleteCashMovement) Execute(ctx context.Context, gym, actor, id uuid.UUID, options ...DeleteCashMovementOptions) error {
	now := time.Now().UTC()
	var option DeleteCashMovementOptions
	if len(options) > 0 {
		option = options[0]
	}
	option.CorrectionReason = strings.TrimSpace(option.CorrectionReason)
	if utf8.RuneCountInString(option.CorrectionReason) < 3 || utf8.RuneCountInString(option.CorrectionReason) > 200 {
		return shared.NewValidationError(expErrors.ErrCorrectionReasonRequired)
	}
	return uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		m, err := uc.Repo.GetByID(tx, gym, id)
		if err != nil {
			return err
		}
		if !m.CanCorrectPhysical() {
			return shared.NewBusinessError(expErrors.ErrLinkedCashMovement, "")
		}
		if option.ExpectedVersion > 0 && option.ExpectedVersion != m.Version {
			return shared.NewBusinessError(expErrors.ErrVersionConflict, "")
		}
		originalDate := m.MovementOn
		originalRecordedAt := m.CreatedAt
		originalDrawerID := m.CashDrawerID
		if originalDrawerID == uuid.Nil {
			originalDrawerID = gym
		}
		v := m.Version
		m.SoftDelete(now)
		if _, err = uc.Repo.Update(tx, m, v); err != nil {
			return shared.NewUnexpectedError(err)
		}
		if uc.Marker != nil {
			if err = uc.Marker.MarkPhysicalCashAdjustment(ctx, tx, CashSessionAdjustmentInput{
				GymID: gym, ActorUserID: actor, OperationalDate: originalDate,
				OriginalRecordedAt: originalRecordedAt, DrawerID: originalDrawerID,
				Reason: option.CorrectionReason,
			}); err != nil {
				return err
			}
		}
		return recordFinancial(ctx, uc.Audit, tx, gym, actor, "cash_movements", id, audit.ActionDelete, map[string]any{"amount": m.Amount}, now)
	})
}

type ListCashMovementsByDate struct {
	Repo expRepo.CashMovementRepository
	UoW  shared.UnitOfWork
}

type ListCashMovementsInput struct {
	GymID    uuid.UUID
	From     *time.Time
	To       *time.Time
	Status   string
	Page     int
	PageSize int
}

type ListCashMovementsOutput struct {
	Items    []*cashDomain.CashMovement
	Total    int
	Page     int
	PageSize int
}

func NewListCashMovementsByDate(r expRepo.CashMovementRepository, u shared.UnitOfWork) *ListCashMovementsByDate {
	return &ListCashMovementsByDate{r, u}
}
func (uc *ListCashMovementsByDate) Execute(ctx context.Context, gym uuid.UUID, day *time.Time, unclassified bool) ([]*cashDomain.CashMovement, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, shared.NewUnexpectedError(err)
	}
	if unclassified {
		return uc.Repo.ListUnclassified(tx, gym, 200)
	}
	if day == nil {
		return nil, shared.NewValidationError(expErrors.ErrInvalidDate)
	}
	return uc.Repo.ListByDate(tx, gym, *day)
}

// ExecuteList returns the owner's complete, paginated physical-cash history.
// The older Execute method remains for the daily-close client, where a single
// date must continue returning every row without pagination.
func (uc *ListCashMovementsByDate) ExecuteList(ctx context.Context, in ListCashMovementsInput) (*ListCashMovementsOutput, error) {
	status := strings.ToLower(strings.TrimSpace(in.Status))
	if status == "" {
		status = "all"
	}
	switch status {
	case "all", "pending", "classified", cashDomain.Unclassified,
		cashDomain.AsExpense, cashDomain.AsInventoryPurchase, cashDomain.NonOperating:
	default:
		return nil, shared.NewValidationError(expErrors.ErrInvalidCashMovementStatus)
	}
	if in.From != nil && in.To != nil && dateOnly(*in.From).After(dateOnly(*in.To)) {
		return nil, shared.NewValidationError(expErrors.ErrInvalidDate)
	}
	page := in.Page
	if page < 1 {
		page = 1
	}
	pageSize := in.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, shared.NewUnexpectedError(err)
	}
	rows, total, err := uc.Repo.List(tx, expRepo.CashMovementListQuery{
		GymID: in.GymID, From: in.From, To: in.To, Status: status,
		Page: page, PageSize: pageSize,
	})
	if err != nil {
		return nil, shared.NewUnexpectedError(err)
	}
	return &ListCashMovementsOutput{Items: rows, Total: total, Page: page, PageSize: pageSize}, nil
}

type ClassifyCashMovementInput struct {
	GymID, ActorUserID, MovementID    uuid.UUID
	AsNonOperating                    bool
	PaidOn                            time.Time
	Category                          string
	PayeeName, Description, Reference *string
	Classification                    string
}
type ClassifyCashMovement struct {
	Movements expRepo.CashMovementRepository
	Expenses  expRepo.ExpenseRepository
	UoW       shared.UnitOfWork
	Audit     audit.Recorder
	Policy    OperationalPolicy
}

func NewClassifyCashMovement(m expRepo.CashMovementRepository, e expRepo.ExpenseRepository, u shared.UnitOfWork, a audit.Recorder, p OperationalPolicy) *ClassifyCashMovement {
	return &ClassifyCashMovement{m, e, u, a, p}
}
func (uc *ClassifyCashMovement) Execute(ctx context.Context, in ClassifyCashMovementInput) (*expenseDomain.Expense, error) {
	now := time.Now().UTC()
	var out *expenseDomain.Expense
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		m, err := uc.Movements.GetByID(tx, in.GymID, in.MovementID)
		if err != nil {
			return err
		}
		if m.MovementType != cashDomain.CashOut {
			return shared.NewBusinessError(expErrors.ErrCashInCannotBeClassified, "")
		}
		if m.ClassificationStatus == cashDomain.AsExpense && m.ExpenseID != nil {
			out, err = uc.Expenses.GetByID(tx, in.GymID, *m.ExpenseID)
			return err
		}
		if m.ClassificationStatus != cashDomain.Unclassified {
			return shared.NewBusinessError(expErrors.ErrAlreadyClassified, "")
		}
		mv := m.Version
		if in.AsNonOperating {
			if err = m.Classify(nil, cashDomain.NonOperating, now); err != nil {
				return shared.NewValidationError(err)
			}
			if _, err = uc.Movements.Update(tx, m, mv); err != nil {
				return shared.NewUnexpectedError(err)
			}
			return recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "cash_movements", m.ID, "classify_non_operating", map[string]any{"reason": m.Reason}, now)
		}
		paid := in.PaidOn
		if paid.IsZero() {
			paid = m.MovementOn
		}
		if dateOnly(paid) != dateOnly(m.MovementOn) {
			return shared.NewBusinessError(expErrors.ErrLinkedExpense, "la fecha debe coincidir con la salida física")
		}
		// A prior correction leaves an immutable expense tombstone. Include the
		// movement generation so reclassification creates a fresh financial
		// record while retries of the same generation remain deterministic.
		id := uuid.NewSHA1(m.ID, []byte("cash-expense:v"+strconv.Itoa(m.Version)))
		e, err := expenseDomain.NewPaid(expenseDomain.PaidInput{ID: id, GymID: in.GymID, CreatedBy: in.ActorUserID, PaidOn: paid, Amount: m.Amount, Category: in.Category, PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashRegister, PayeeName: in.PayeeName, Description: in.Description, Reference: in.Reference, Classification: in.Classification, Source: expenseDomain.SourceCashMovement, CashMovementID: &m.ID, Now: now})
		if err != nil {
			return shared.NewValidationError(err)
		}
		if _, err = uc.Expenses.Create(tx, e); err != nil {
			return shared.NewUnexpectedError(err)
		}
		if err = m.Classify(&e.ID, cashDomain.AsExpense, now); err != nil {
			return shared.NewValidationError(err)
		}
		if _, err = uc.Movements.Update(tx, m, mv); err != nil {
			return shared.NewUnexpectedError(err)
		}
		if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expenses", e.ID, "classify_cash_movement", map[string]any{"cash_movement_id": m.ID, "amount": m.Amount}, now); err != nil {
			return err
		}
		if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "cash_movements", m.ID, "classify_as_expense", map[string]any{"expense_id": e.ID, "classification_status": m.ClassificationStatus}, now); err != nil {
			return err
		}
		out = e
		return nil
	})
	return out, err
}

type UnclassifyCashMovementInput struct {
	GymID, ActorUserID, MovementID uuid.UUID
	ActorRole                      string
	ExpectedVersion                int
	CorrectionReason               string
}

type UnclassifyCashMovement struct {
	Occurrences expRepo.OccurrenceRepository
	Movements   expRepo.CashMovementRepository
	Expenses    expRepo.ExpenseRepository
	UoW         shared.UnitOfWork
	Audit       audit.Recorder
}

func NewUnclassifyCashMovement(m expRepo.CashMovementRepository, u shared.UnitOfWork, a audit.Recorder) *UnclassifyCashMovement {
	return &UnclassifyCashMovement{Movements: m, UoW: u, Audit: a}
}

func (uc *UnclassifyCashMovement) WithExpenses(expenses expRepo.ExpenseRepository) *UnclassifyCashMovement {
	uc.Expenses = expenses
	return uc
}

func (uc *UnclassifyCashMovement) WithOccurrences(occurrences expRepo.OccurrenceRepository) *UnclassifyCashMovement {
	uc.Occurrences = occurrences
	return uc
}

func (uc *UnclassifyCashMovement) Execute(ctx context.Context, in UnclassifyCashMovementInput) (*cashDomain.CashMovement, error) {
	if in.ActorRole != "owner" {
		return nil, shared.NewBusinessError(expErrors.ErrCorrectionOwnerRequired, "")
	}
	reason := strings.TrimSpace(in.CorrectionReason)
	if utf8.RuneCountInString(reason) < 3 || utf8.RuneCountInString(reason) > 200 {
		return nil, shared.NewValidationError(expErrors.ErrCorrectionReasonRequired)
	}
	var out *cashDomain.CashMovement
	now := time.Now().UTC()
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		m, err := uc.Movements.GetByID(tx, in.GymID, in.MovementID)
		if err != nil {
			return err
		}
		if in.ExpectedVersion > 0 && in.ExpectedVersion != m.Version {
			return shared.NewBusinessError(expErrors.ErrVersionConflict, "")
		}
		if m.ClassificationStatus == cashDomain.Unclassified {
			out = m
			return nil
		}
		if m.ClassificationStatus != cashDomain.NonOperating && m.ClassificationStatus != cashDomain.AsExpense {
			return shared.NewBusinessError(expErrors.ErrLinkedCashMovement, "")
		}
		before := m.ClassificationStatus
		var voidedExpenseID *uuid.UUID
		if before == cashDomain.AsExpense {
			if m.ExpenseID == nil || uc.Expenses == nil {
				return shared.NewBusinessError(expErrors.ErrLinkedCashMovement, "")
			}
			e, expenseErr := uc.Expenses.GetByID(tx, in.GymID, *m.ExpenseID)
			if expenseErr != nil {
				return expenseErr
			}
			if e.CashMovementID == nil || *e.CashMovementID != m.ID {
				return shared.NewBusinessError(expErrors.ErrLinkedCashMovement, "")
			}
			if e.RecurringOccurrenceID != nil {
				if uc.Occurrences == nil {
					return shared.NewBusinessError(expErrors.ErrLinkedExpense, "")
				}
				o, getErr := uc.Occurrences.GetByID(tx, in.GymID, *e.RecurringOccurrenceID)
				if getErr != nil {
					return getErr
				}
				if o.Status != "paid" || o.ExpenseID == nil || *o.ExpenseID != e.ID {
					return shared.NewBusinessError(expErrors.ErrLinkedExpense, "")
				}
				version := o.Version
				if getErr = o.Reopen(now); getErr != nil {
					return getErr
				}
				if _, getErr = uc.Occurrences.Update(tx, o, version); getErr != nil {
					return getErr
				}
				if getErr = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expense_occurrences", o.ID, "reopen", map[string]any{"expense_id": e.ID, "correction_reason": reason}, now); getErr != nil {
					return getErr
				}
			}
			expenseID := e.ID
			voidedExpenseID = &expenseID
			e.DetachCashMovementForCorrection()
			e.SoftDelete(now)
			if _, expenseErr = uc.Expenses.Update(tx, e); expenseErr != nil {
				return shared.NewUnexpectedError(expenseErr)
			}
			if expenseErr = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expenses", e.ID,
				"void_for_cash_reclassification", map[string]any{
					"cash_movement_id": m.ID, "amount": e.Amount, "category": e.Category,
					"correction_reason": reason,
				}, now); expenseErr != nil {
				return expenseErr
			}
		} else if m.ExpenseID != nil {
			return shared.NewBusinessError(expErrors.ErrLinkedCashMovement, "")
		}
		v := m.Version
		m.Unclassify(now)
		if _, err = uc.Movements.Update(tx, m, v); err != nil {
			return shared.NewUnexpectedError(err)
		}
		action := "unclassify_non_operating"
		if before == cashDomain.AsExpense {
			action = "unclassify_expense"
		}
		if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "cash_movements", m.ID,
			action, map[string]any{"classification_before": before, "classification_after": m.ClassificationStatus,
				"voided_expense_id": voidedExpenseID, "correction_reason": reason}, now); err != nil {
			return err
		}
		out = m
		return nil
	})
	return out, err
}

func recordFinancial(ctx context.Context, r audit.Recorder, tx shared.Transaction, gym, actor uuid.UUID, typ string, id uuid.UUID, action string, changes any, at time.Time) error {
	if err := r.Record(ctx, tx, audit.Entry{GymID: gym, EntityType: typ, EntityID: id, Action: action, ActorUserID: &actor, Changes: changes, IPAddress: audit.IPFromContext(ctx), UserAgent: audit.UAFromContext(ctx), At: at}); err != nil {
		return shared.NewUnexpectedError(err)
	}
	return nil
}
