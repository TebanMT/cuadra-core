// Package expense holds the Expense aggregate of the expenses BC.
//
// Un Expense es siempre una erogación pagada. Las obligaciones pendientes
// viven en recurring.Occurrence; las compras para reventa en stock_movements.
// No modela deducibilidad, CFDI ni contabilidad fiscal.
package expense

import (
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
)

// Category enumera las categorías permitidas. Sin "custom" en MVP — si
// el dueño necesita más granularidad, se agrega aquí + check constraint
// del schema (no se delega al cliente para evitar drift entre gyms).
const (
	CategoryRent                 = "renta"
	CategoryUtilities            = "servicios"
	CategoryMaintenance          = "mantenimiento"
	CategoryPayroll              = "nomina"
	CategoryMarketing            = "marketing"
	CategoryNonInventorySupplies = "insumos_no_inventariables"
	CategoryTaxesAndPermits      = "impuestos_y_permisos"
	CategoryOther                = "otros"
	// Legacy values are readable during rollout, but new writes are
	// normalized to the canonical categories below.
	CategorySalaries          = "sueldos"
	CategoryExternalInventory = "mercaderia_externa"
)

func ValidCategories() []string {
	return []string{
		CategoryRent, CategoryUtilities, CategoryPayroll, CategoryMaintenance,
		CategoryMarketing, CategoryNonInventorySupplies, CategoryTaxesAndPermits, CategoryOther,
	}
}

func isValidCategory(c string) bool {
	if c == CategorySalaries || c == CategoryExternalInventory {
		return true
	}
	for _, v := range ValidCategories() {
		if v == c {
			return true
		}
	}
	return false
}

// Payment methods aceptados — mismos buckets que payments para que los
// reportes "cash vs no-cash" sumen consistentemente cross-BC.
const (
	PaymentCash     = "cash"
	PaymentTransfer = "transfer"
	PaymentCard     = "card"
)

func isValidPaymentMethod(m string) bool {
	return m == PaymentCash || m == PaymentTransfer || m == PaymentCard
}

// PaidFrom answers a different question than PaymentMethod. A rent payment
// can be made in cash but come from the gym's accumulated fund rather than
// from today's reception drawer.
const (
	// cash_drawer is the canonical API value shared with inventory purchases.
	// cash_register remains the persisted legacy value for the current schema;
	// SetPaidFrom accepts either and stores only cash_register.
	PaidFromCashDrawer   = "cash_drawer"
	PaidFromCashRegister = "cash_register"
	PaidFromGymFund      = "gym_fund"
	PaidFromExternal     = "external"
)

const maxDescriptionLen = 200

const maxAmount = 9999999999.99

const (
	ClassificationFixed    = "fixed"
	ClassificationVariable = "variable"
	SourceManual           = "manual"
	SourceRecurring        = "recurring"
	SourceCashMovement     = "cash_movement"
)

// Expense — egreso pagado capturado por el dueño Plus. Money se mantiene
// como float64 aquí; el mapper SQLite convierte a cents en el edge.
type Expense struct {
	ID                    uuid.UUID
	GymID                 uuid.UUID
	Version               int
	PaidOn                time.Time // fecha local del gimnasio, sin hora
	ExpenseDate           time.Time // alias histórico de storage; mantener igual a PaidOn
	Amount                float64
	Category              string
	PayeeName             *string
	Description           *string
	Reference             *string
	PaymentMethod         string
	PaidFrom              string
	Classification        string
	Source                string
	RecurringOccurrenceID *uuid.UUID
	CashMovementID        *uuid.UUID
	// CashDrawerID is a derived read field hydrated from CashMovementID. It is
	// intentionally not persisted on expenses: the physical ledger remains
	// the single source of truth for drawer attribution.
	CashDrawerID *uuid.UUID
	CreatedBy    uuid.UUID
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
}

// New construye un Expense aplicando validaciones de dominio.
func New(id, gymID, createdBy uuid.UUID, expenseDate time.Time, amount float64,
	category, paymentMethod string, description *string, now time.Time) (*Expense, error) {
	e := &Expense{
		ID:             id,
		GymID:          gymID,
		Version:        1,
		CreatedBy:      createdBy,
		CreatedAt:      now,
		UpdatedAt:      now,
		Classification: ClassificationVariable,
		Source:         SourceManual,
	}
	if err := e.applyFields(expenseDate, amount, category, paymentMethod, description); err != nil {
		return nil, err
	}
	if err := e.SetPaidFrom(""); err != nil {
		return nil, err
	}
	return e, nil
}

type PaidInput struct {
	ID, GymID, CreatedBy                                      uuid.UUID
	PaidOn                                                    time.Time
	Amount                                                    float64
	Category, PaymentMethod, PaidFrom, Classification, Source string
	PayeeName, Description, Reference                         *string
	RecurringOccurrenceID, CashMovementID                     *uuid.UUID
	Now                                                       time.Time
}

func NewPaid(in PaidInput) (*Expense, error) {
	e, err := New(in.ID, in.GymID, in.CreatedBy, in.PaidOn, in.Amount, in.Category, in.PaymentMethod, in.Description, in.Now)
	if err != nil {
		return nil, err
	}
	if err := e.SetMetadata(in.PayeeName, in.Reference, in.Classification, in.Source, in.RecurringOccurrenceID, in.CashMovementID); err != nil {
		return nil, err
	}
	if err := e.SetPaidFrom(in.PaidFrom); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Expense) SetPaidFrom(paidFrom string) error {
	paidFrom = NormalizePaidFrom(paidFrom)
	if paidFrom == "" {
		if e.PaymentMethod == PaymentCash {
			paidFrom = PaidFromCashRegister
		} else {
			paidFrom = PaidFromGymFund
		}
	}
	if paidFrom != PaidFromCashRegister && paidFrom != PaidFromGymFund && paidFrom != PaidFromExternal {
		return expErrors.ErrInvalidPaidFrom
	}
	if paidFrom == PaidFromCashRegister && e.PaymentMethod != PaymentCash {
		return expErrors.ErrInvalidPaidFrom
	}
	e.PaidFrom = paidFrom
	return nil
}

// NormalizePaidFrom accepts the one-release legacy wire alias and returns the
// storage/domain representation used by the expenses schema.
func NormalizePaidFrom(paidFrom string) string {
	paidFrom = strings.TrimSpace(paidFrom)
	if paidFrom == PaidFromCashDrawer {
		return PaidFromCashRegister
	}
	return paidFrom
}

// PaidFromWire exposes one vocabulary across expenses and purchases.
func PaidFromWire(paidFrom string) string {
	if NormalizePaidFrom(paidFrom) == PaidFromCashRegister {
		return PaidFromCashDrawer
	}
	return NormalizePaidFrom(paidFrom)
}

func (e *Expense) SetMetadata(payee, reference *string, classification, source string, occurrenceID, movementID *uuid.UUID) error {
	var err error
	if e.PayeeName, err = cleanText(payee, 120); err != nil {
		return expErrors.ErrInvalidPayee
	}
	if e.Reference, err = cleanText(reference, 120); err != nil {
		return expErrors.ErrInvalidReference
	}
	if classification == "" {
		classification = ClassificationVariable
	}
	if classification != ClassificationFixed && classification != ClassificationVariable {
		return expErrors.ErrInvalidClassification
	}
	if source == "" {
		source = SourceManual
	}
	if source != SourceManual && source != SourceRecurring && source != SourceCashMovement {
		return expErrors.ErrInvalidSource
	}
	if source == SourceRecurring && occurrenceID == nil {
		return expErrors.ErrInvalidSource
	}
	if source == SourceCashMovement && movementID == nil {
		return expErrors.ErrInvalidSource
	}
	e.Classification = classification
	e.Source = source
	e.RecurringOccurrenceID = occurrenceID
	e.CashMovementID = movementID
	return nil
}

// Update muta los campos editables del gasto.
func (e *Expense) Update(expenseDate time.Time, amount float64,
	category, paymentMethod string, description *string, now time.Time) error {
	if err := e.applyFields(expenseDate, amount, category, paymentMethod, description); err != nil {
		return err
	}
	e.Version++
	e.UpdatedAt = now
	return nil
}

// SoftDelete marca el gasto como borrado preservando trazabilidad.
func (e *Expense) SoftDelete(now time.Time) {
	if e.DeletedAt != nil {
		return
	}
	e.DeletedAt = &now
	e.Version++
	e.UpdatedAt = now
}

// DetachRecurringOccurrenceForCorrection frees the live 1:1 occurrence key
// before this expense becomes a tombstone. The audit entry retains the former
// relationship, while a later corrected payment can create a new immutable
// financial record for the same occurrence.
func (e *Expense) DetachRecurringOccurrenceForCorrection() {
	e.RecurringOccurrenceID = nil
}

// DetachCashMovementForCorrection releases the live 1:1 key before the
// expense is tombstoned. The physical movement remains immutable and can be
// classified again into a new expense after the owner corrects a mistake.
func (e *Expense) DetachCashMovementForCorrection() {
	e.CashMovementID = nil
	e.CashDrawerID = nil
}

func (e *Expense) applyFields(expenseDate time.Time, amount float64,
	category, paymentMethod string, description *string) error {
	if expenseDate.IsZero() {
		return expErrors.ErrInvalidDate
	}
	// Normalizamos a fecha pura (00:00 UTC) — el ticket guarda YYYY-MM-DD,
	// la hora no aporta.
	e.ExpenseDate = time.Date(expenseDate.Year(), expenseDate.Month(), expenseDate.Day(), 0, 0, 0, 0, time.UTC)
	e.PaidOn = e.ExpenseDate
	scaled := amount * 100
	centTolerance := math.Max(1e-7, math.Abs(scaled)*1e-15)
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 || amount > maxAmount || math.Abs(scaled-math.Round(scaled)) > centTolerance {
		return expErrors.ErrInvalidAmount
	}
	e.Amount = amount
	if !isValidCategory(category) {
		return expErrors.ErrInvalidCategory
	}
	if category == CategorySalaries {
		category = CategoryPayroll
	}
	if category == CategoryExternalInventory {
		category = CategoryNonInventorySupplies
	}
	e.Category = category
	if !isValidPaymentMethod(paymentMethod) {
		return expErrors.ErrInvalidPaymentMethod
	}
	e.PaymentMethod = paymentMethod
	if description != nil {
		d := strings.TrimSpace(*description)
		if d == "" {
			description = nil
		} else {
			if utf8.RuneCountInString(d) > maxDescriptionLen {
				return expErrors.ErrInvalidDescription
			}
			description = &d
		}
	}
	e.Description = description
	return nil
}

func cleanText(v *string, max int) (*string, error) {
	if v == nil {
		return nil, nil
	}
	s := strings.TrimSpace(*v)
	if s == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(s) > max {
		return nil, expErrors.ErrInvalidDescription
	}
	return &s, nil
}
