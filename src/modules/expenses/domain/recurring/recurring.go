// Package recurring contains the deterministic calendar rules for future
// expense obligations. Occurrences are obligations, never payments.
package recurring

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	"github.com/google/uuid"
)

const (
	Weekly      = "weekly"
	Every14Days = "every_14_days"
	Semimonthly = "semimonthly"
	Monthly     = "monthly"
	Bimonthly   = "bimonthly"
	Quarterly   = "quarterly"
	Semiannual  = "semiannual"
	Annual      = "annual"
	Pending     = "pending"
	Paid        = "paid"
	Skipped     = "skipped"
)

var ErrResolved = errors.New("este vencimiento ya fue resuelto")

type Template struct {
	ID, GymID                                uuid.UUID
	Version                                  int
	Name                                     string
	PayeeName                                *string
	Category                                 string
	ExpectedAmount                           float64
	PaymentMethod, Classification, Frequency string
	StartsOn                                 time.Time
	EndsOn                                   *time.Time
	NextDueOn                                time.Time
	Active                                   bool
	CreatedBy                                uuid.UUID
	CreatedAt, UpdatedAt                     time.Time
	DeletedAt                                *time.Time
}

type TemplateInput struct {
	ID, GymID, CreatedBy                     uuid.UUID
	Name                                     string
	PayeeName                                *string
	Category                                 string
	ExpectedAmount                           float64
	PaymentMethod, Classification, Frequency string
	StartsOn                                 time.Time
	EndsOn                                   *time.Time
	Now                                      time.Time
}

func NewTemplate(in TemplateInput) (*Template, error) {
	t := &Template{ID: in.ID, GymID: in.GymID, Version: 1, CreatedBy: in.CreatedBy, CreatedAt: in.Now, UpdatedAt: in.Now, Active: true}
	if err := t.apply(in); err != nil {
		return nil, err
	}
	t.NextDueOn = t.StartsOn
	return t, nil
}
func (t *Template) Update(in TemplateInput, now time.Time) error {
	if err := t.apply(in); err != nil {
		return err
	}
	t.Version++
	t.UpdatedAt = now
	return nil
}
func (t *Template) Deactivate(now time.Time) {
	if t.Active {
		t.Active = false
		t.Version++
		t.UpdatedAt = now
	}
}
func (t *Template) Reactivate(now time.Time) {
	if !t.Active && t.DeletedAt == nil {
		t.Active = true
		t.Version++
		t.UpdatedAt = now
	}
}
func (t *Template) apply(in TemplateInput) error {
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > 120 {
		return errors.New("nombre inválido")
	}
	scaled := in.ExpectedAmount * 100
	centTolerance := math.Max(1e-7, math.Abs(scaled)*1e-15)
	if math.IsNaN(in.ExpectedAmount) || math.IsInf(in.ExpectedAmount, 0) || in.ExpectedAmount <= 0 || in.ExpectedAmount > 9999999999.99 || math.Abs(scaled-math.Round(scaled)) > centTolerance {
		return errors.New("monto esperado inválido")
	}
	validCat := false
	for _, c := range expenseDomain.ValidCategories() {
		if c == in.Category {
			validCat = true
		}
	}
	if !validCat {
		return errors.New("categoría inválida")
	}
	if in.PaymentMethod != expenseDomain.PaymentCash && in.PaymentMethod != expenseDomain.PaymentTransfer && in.PaymentMethod != expenseDomain.PaymentCard {
		return errors.New("método inválido")
	}
	if in.Classification != expenseDomain.ClassificationFixed && in.Classification != expenseDomain.ClassificationVariable {
		return errors.New("clasificación inválida")
	}
	if _, err := NextDue(in.StartsOn, in.Frequency, in.StartsOn.Day()); err != nil {
		return err
	}
	if in.EndsOn != nil && in.EndsOn.Before(in.StartsOn) {
		return errors.New("fecha final anterior al inicio")
	}
	if in.PayeeName != nil {
		p := strings.TrimSpace(*in.PayeeName)
		if utf8.RuneCountInString(p) > 120 {
			return errors.New("proveedor demasiado largo")
		}
		if p == "" {
			in.PayeeName = nil
		} else {
			in.PayeeName = &p
		}
	}
	t.Name = name
	t.PayeeName = in.PayeeName
	t.Category = in.Category
	t.ExpectedAmount = in.ExpectedAmount
	t.PaymentMethod = in.PaymentMethod
	t.Classification = in.Classification
	t.Frequency = in.Frequency
	t.StartsOn = date(in.StartsOn)
	if in.EndsOn != nil {
		x := date(*in.EndsOn)
		t.EndsOn = &x
	} else {
		t.EndsOn = nil
	}
	return nil
}

func NewOccurrence(t *Template, due time.Time, now time.Time) *Occurrence {
	return &Occurrence{ID: OccurrenceID(t.GymID, t.ID, due), GymID: t.GymID, TemplateID: t.ID, Version: 1, DueOn: date(due), ExpectedAmount: t.ExpectedAmount, Category: t.Category, PayeeName: t.PayeeName, PaymentMethod: t.PaymentMethod, Classification: t.Classification, Status: Pending, CreatedAt: now, UpdatedAt: now}
}

type Occurrence struct {
	// Read-only details resolved from the template and the actual paid expense.
	Name                  string
	PaidAmount            *float64
	PaidOn                *time.Time
	ID, GymID, TemplateID uuid.UUID
	Version               int
	DueOn                 time.Time
	ExpectedAmount        float64
	Category              string
	PayeeName             *string
	PaymentMethod         string
	Classification        string
	Status                string
	ExpenseID, ResolvedBy *uuid.UUID
	ResolvedAt            *time.Time
	SkipReason            *string
	CreatedAt, UpdatedAt  time.Time
	DeletedAt             *time.Time
}

// OccurrenceID makes cloud and sidecar converge on the same primary key.
// The unique database key remains the final guard against concurrent inserts.
func OccurrenceID(gymID, templateID uuid.UUID, dueOn time.Time) uuid.UUID {
	name := gymID.String() + ":" + templateID.String() + ":" + dueOn.Format("2006-01-02")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name))
}

func (o *Occurrence) MarkPaid(expenseID, actor uuid.UUID, at time.Time) error {
	if o.Status == Paid && o.ExpenseID != nil && *o.ExpenseID == expenseID {
		return nil
	}
	if o.Status != Pending {
		return ErrResolved
	}
	o.Status = Paid
	o.ExpenseID = &expenseID
	o.ResolvedBy = &actor
	o.ResolvedAt = &at
	o.Version++
	o.UpdatedAt = at
	return nil
}

func (o *Occurrence) Skip(actor uuid.UUID, reason string, at time.Time) error {
	if o.Status == Skipped {
		return nil
	}
	if o.Status != Pending {
		return ErrResolved
	}
	o.Status = Skipped
	o.ResolvedBy = &actor
	o.ResolvedAt = &at
	o.SkipReason = &reason
	o.Version++
	o.UpdatedAt = at
	return nil
}

// Reopen returns a resolved due to pending without erasing its audit trail.
// The application service coordinates any linked paid Expense first.
func (o *Occurrence) Reopen(at time.Time) error {
	if o.Status == Pending {
		return nil
	}
	if o.Status != Paid && o.Status != Skipped {
		return ErrResolved
	}
	o.Status = Pending
	o.ExpenseID = nil
	o.ResolvedBy = nil
	o.ResolvedAt = nil
	o.SkipReason = nil
	o.Version++
	o.UpdatedAt = at.UTC()
	return nil
}

// NextDue uses calendar dates only. Monthly-like schedules clamp day 29-31
// to the last available day; semimonthly means the 15th and month-end and is
// intentionally distinct from a rolling fourteen-day interval.
func NextDue(current time.Time, frequency string, anchorDay int) (time.Time, error) {
	d := date(current)
	switch frequency {
	case Weekly:
		return d.AddDate(0, 0, 7), nil
	case Every14Days:
		return d.AddDate(0, 0, 14), nil
	case Semimonthly:
		if d.Day() < 15 {
			return time.Date(d.Year(), d.Month(), 15, 0, 0, 0, 0, time.UTC), nil
		}
		last := lastDay(d.Year(), d.Month())
		if d.Day() < last {
			return time.Date(d.Year(), d.Month(), last, 0, 0, 0, 0, time.UTC), nil
		}
		return time.Date(d.Year(), d.Month()+1, 15, 0, 0, 0, 0, time.UTC), nil
	case Monthly:
		return addMonthsClamped(d, 1, anchorDay), nil
	case Bimonthly:
		return addMonthsClamped(d, 2, anchorDay), nil
	case Quarterly:
		return addMonthsClamped(d, 3, anchorDay), nil
	case Semiannual:
		return addMonthsClamped(d, 6, anchorDay), nil
	case Annual:
		return addMonthsClamped(d, 12, anchorDay), nil
	default:
		return time.Time{}, errors.New("frecuencia inválida")
	}
}

// FirstDueOnOrAfter projects a rule onto its first calendar due date at or
// after target. It is used when a rule is explicitly reprogrammed; generated
// occurrences remain immutable history and prevent silent schedule rewrites.
func FirstDueOnOrAfter(startsOn time.Time, frequency string, target time.Time) (time.Time, error) {
	due := date(startsOn)
	target = date(target)
	anchor := due.Day()
	for steps := 0; due.Before(target); steps++ {
		if steps >= 100000 {
			return time.Time{}, errors.New("el rango de la regla es demasiado amplio")
		}
		next, err := NextDue(due, frequency, anchor)
		if err != nil {
			return time.Time{}, err
		}
		due = next
	}
	return due, nil
}

func date(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
func lastDay(y int, m time.Month) int { return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day() }
func addMonthsClamped(d time.Time, n, anchor int) time.Time {
	first := time.Date(d.Year(), d.Month()+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	if anchor < 1 {
		anchor = d.Day()
	}
	if anchor > lastDay(first.Year(), first.Month()) {
		anchor = lastDay(first.Year(), first.Month())
	}
	return time.Date(first.Year(), first.Month(), anchor, 0, 0, 0, 0, time.UTC)
}
