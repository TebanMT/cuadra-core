package cashmovement

import (
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	"github.com/google/uuid"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	CashIn              = "cash_in"
	CashOut             = "cash_out"
	Unclassified        = "unclassified"
	AsExpense           = "expense"
	AsInventoryPurchase = "inventory_purchase"
	NonOperating        = "non_operating"
)

type CashMovement struct {
	ID, GymID            uuid.UUID
	CashDrawerID         uuid.UUID
	Version              int
	MovementOn           time.Time
	Amount               float64
	MovementType, Reason string
	OperatorID           uuid.UUID
	ExpenseID            *uuid.UUID
	ClassificationStatus string
	CreatedAt, UpdatedAt time.Time
	DeletedAt            *time.Time
}

func New(id, gymID, operatorID uuid.UUID, on time.Time, amount float64, kind, reason string, now time.Time) (*CashMovement, error) {
	m := &CashMovement{ID: id, GymID: gymID, CashDrawerID: gymID, Version: 1, OperatorID: operatorID, CreatedAt: now, UpdatedAt: now, ClassificationStatus: Unclassified}
	if err := m.apply(on, amount, kind, reason); err != nil {
		return nil, err
	}
	// A manual cash-in describes physical float/contribution, not business
	// income. Real income must be registered through billing (concept=other)
	// so it has a payment method, date and auditable financial semantics.
	if kind == CashIn {
		m.ClassificationStatus = NonOperating
	}
	return m, nil
}

func (m *CashMovement) WithCashDrawer(drawerID uuid.UUID) *CashMovement {
	if m != nil && drawerID != uuid.Nil {
		m.CashDrawerID = drawerID
	}
	return m
}
func (m *CashMovement) Update(on time.Time, amount float64, kind, reason string, now time.Time) error {
	previousKind := m.MovementType
	if err := m.apply(on, amount, kind, reason); err != nil {
		return err
	}
	if previousKind != kind && m.ExpenseID == nil {
		if kind == CashIn {
			m.ClassificationStatus = NonOperating
		} else if previousKind == CashIn && m.ClassificationStatus == NonOperating {
			m.ClassificationStatus = Unclassified
		}
	}
	m.Version++
	m.UpdatedAt = now
	return nil
}

// CanCorrectPhysical permits edits only while the movement has no economic
// owner. Cash-out waits for classification; cash-in is non-operating by
// construction but remains physically correctable.
func (m *CashMovement) CanCorrectPhysical() bool {
	return m != nil && m.ExpenseID == nil &&
		((m.MovementType == CashOut && m.ClassificationStatus == Unclassified) ||
			(m.MovementType == CashIn && m.ClassificationStatus == NonOperating))
}

func (m *CashMovement) Classify(expenseID *uuid.UUID, status string, now time.Time) error {
	if m.MovementType != CashOut {
		return expErrors.ErrCashInCannotBeClassified
	}
	if m.ClassificationStatus != Unclassified {
		return expErrors.ErrAlreadyClassified
	}
	if status != AsExpense && status != NonOperating {
		return expErrors.ErrAlreadyClassified
	}
	if status == AsExpense && expenseID == nil {
		return expErrors.ErrAlreadyClassified
	}
	m.ExpenseID = expenseID
	m.ClassificationStatus = status
	m.Version++
	m.UpdatedAt = now
	return nil
}

// ClassifyInventoryPurchase marks the physical outflow as owned by the
// inventory-purchase aggregate. The purchase keeps the 1:1 foreign key, so an
// expense id would be both redundant and misleading here.
func (m *CashMovement) ClassifyInventoryPurchase(now time.Time) error {
	if m.ClassificationStatus != Unclassified || m.MovementType != CashOut {
		return expErrors.ErrAlreadyClassified
	}
	m.ExpenseID = nil
	m.ClassificationStatus = AsInventoryPurchase
	m.Version++
	m.UpdatedAt = now
	return nil
}
func (m *CashMovement) Unclassify(now time.Time) {
	m.ExpenseID = nil
	if m.MovementType == CashIn {
		m.ClassificationStatus = NonOperating
	} else {
		m.ClassificationStatus = Unclassified
	}
	m.Version++
	m.UpdatedAt = now
}
func (m *CashMovement) SoftDelete(now time.Time) {
	if m.DeletedAt == nil {
		m.DeletedAt = &now
		m.Version++
		m.UpdatedAt = now
	}
}
func (m *CashMovement) apply(on time.Time, amount float64, kind, reason string) error {
	if on.IsZero() {
		return expErrors.ErrInvalidDate
	}
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 || amount > 9999999999.99 || math.Abs(amount*100-math.Round(amount*100)) > 1e-7 {
		return expErrors.ErrInvalidAmount
	}
	if kind != CashIn && kind != CashOut {
		return expErrors.ErrInvalidPaymentMethod
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > 200 {
		return expErrors.ErrInvalidDescription
	}
	m.MovementOn = time.Date(on.Year(), on.Month(), on.Day(), 0, 0, 0, 0, time.UTC)
	m.Amount = amount
	m.MovementType = kind
	m.Reason = reason
	return nil
}
