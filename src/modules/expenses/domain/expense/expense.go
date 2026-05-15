// Package expense holds the Expense aggregate of the expenses BC.
//
// Gasto general del gimnasio. Tipos contemplados: renta, servicios,
// mantenimiento, sueldos, marketing, mercadería externa, otros.
// NO incluye recurrencias, foto de recibo, integración fiscal ni marcar
// pagado/pendiente — esos casos viven en un tier "Tinta Pro" futuro.
package expense

import (
	"strings"
	"time"

	"github.com/google/uuid"

	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
)

// Category enumera las 7 categorías permitidas. Sin "custom" en MVP — si
// el dueño necesita más granularidad, se agrega aquí + check constraint
// del schema (no se delega al cliente para evitar drift entre gyms).
const (
	CategoryRent              = "renta"
	CategoryUtilities         = "servicios"
	CategoryMaintenance       = "mantenimiento"
	CategorySalaries          = "sueldos"
	CategoryMarketing         = "marketing"
	CategoryExternalInventory = "mercaderia_externa"
	CategoryOther             = "otros"
)

func ValidCategories() []string {
	return []string{
		CategoryRent, CategoryUtilities, CategoryMaintenance, CategorySalaries,
		CategoryMarketing, CategoryExternalInventory, CategoryOther,
	}
}

func isValidCategory(c string) bool {
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

const maxDescriptionLen = 200

// Expense — un egreso general capturado por el operador. Money se
// mantiene como float64 aquí; el mapper SQLite convierte a cents al
// edge, igual que products.Price.
type Expense struct {
	ID            uuid.UUID
	GymID         uuid.UUID
	Version       int
	ExpenseDate   time.Time // solo fecha (YYYY-MM-DD), sin hora
	Amount        float64
	Category      string
	Description   *string
	PaymentMethod string
	CreatedBy     uuid.UUID
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time
}

// New construye un Expense aplicando validaciones de dominio.
func New(id, gymID, createdBy uuid.UUID, expenseDate time.Time, amount float64,
	category, paymentMethod string, description *string, now time.Time) (*Expense, error) {
	e := &Expense{
		ID:        id,
		GymID:     gymID,
		Version:   1,
		CreatedBy: createdBy,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := e.applyFields(expenseDate, amount, category, paymentMethod, description); err != nil {
		return nil, err
	}
	return e, nil
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

func (e *Expense) applyFields(expenseDate time.Time, amount float64,
	category, paymentMethod string, description *string) error {
	if expenseDate.IsZero() {
		return expErrors.ErrInvalidDate
	}
	// Normalizamos a fecha pura (00:00 UTC) — el ticket guarda YYYY-MM-DD,
	// la hora no aporta.
	e.ExpenseDate = time.Date(expenseDate.Year(), expenseDate.Month(), expenseDate.Day(), 0, 0, 0, 0, time.UTC)
	if amount <= 0 {
		return expErrors.ErrInvalidAmount
	}
	e.Amount = amount
	if !isValidCategory(category) {
		return expErrors.ErrInvalidCategory
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
			if len(d) > maxDescriptionLen {
				return expErrors.ErrInvalidDescription
			}
			description = &d
		}
	}
	e.Description = description
	return nil
}
