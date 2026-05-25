// Package membership holds the Membership aggregate. A Membership is the
// active "subscription" of a Member to a MembershipType; renewals create a
// new row and mark the previous one as `replaced` (audit-friendly + the
// uq_memberships_member_active partial unique index is preserved).
package membership

import (
	"strings"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
)

// Status mirrors chk_memberships_status.
const (
	StatusActive         = "active"
	StatusExpired        = "expired"
	StatusReplaced       = "replaced"
	StatusCancelled      = "cancelled"
	StatusPendingPayment = "pending_payment"
)

// Membership is the active subscription. Snapshot fields are FROZEN at
// creation — see DA-11.1 / ADR-002 §3.6: a MembershipType price/duration
// change does NOT propagate to existing Memberships.
//
// ExpiryDate is nil when Status == pending_payment (socio inscrito pero sin
// pago todavía). El primer pago — completo o parcial — activa la membresía
// y setea ExpiryDate. Para cualquier otro status, ExpiryDate es no-nil.
//
// DurationMonthsSnapshot: si !nil, esta membership se vendió como N meses
// naturales (vs días corridos). Lo usamos en Activate() y para auditar
// renovaciones que vuelven al mismo plan. Mirror del nuevo
// `duration_months` en membership_types (migración 027/022).
type Membership struct {
	ID                     uuid.UUID
	GymID                  uuid.UUID
	Version                int
	MemberID               uuid.UUID
	MembershipTypeID       uuid.UUID
	TypeNameSnapshot       string
	PriceSnapshot          float64
	DurationDaysSnapshot   int
	DurationMonthsSnapshot *int
	StartDate              time.Time
	ExpiryDate             *time.Time
	Status                 string
	ReplacedBy             *uuid.UUID
	CreatedAt              time.Time
	UpdatedAt              time.Time
	DeletedAt              *time.Time
}

// New constructs an ACTIVE Membership with expiry = start + duration. Use
// NewPendingPayment for the "inscrito sin pago" case. The caller is
// responsible for ensuring there's no other active/pending membership for
// this member (the DB unique index will also enforce this).
func New(id uuid.UUID, gymID, memberID uuid.UUID, mt *mtDomain.MembershipType, startDate time.Time, now time.Time) *Membership {
	start := truncateDate(startDate)
	expiry := computeExpiry(start, mt.DurationDays, mt.DurationMonths)
	return &Membership{
		ID:                     id,
		GymID:                  gymID,
		Version:                1,
		MemberID:               memberID,
		MembershipTypeID:       mt.ID,
		TypeNameSnapshot:       mt.Name,
		PriceSnapshot:          mt.Price,
		DurationDaysSnapshot:   mt.DurationDays,
		DurationMonthsSnapshot: cloneIntPtr(mt.DurationMonths),
		StartDate:              start,
		ExpiryDate:             &expiry,
		Status:                 StatusActive,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
}

// NewPendingPayment constructs a Membership inscribed but unpaid. ExpiryDate
// stays nil until the first payment (parcial o completo) activates it via
// Activate(). The snapshot fields are still frozen — el plan elegido al
// inscribir es el que el socio terminará pagando.
func NewPendingPayment(id uuid.UUID, gymID, memberID uuid.UUID, mt *mtDomain.MembershipType, startDate time.Time, now time.Time) *Membership {
	start := truncateDate(startDate)
	return &Membership{
		ID:                     id,
		GymID:                  gymID,
		Version:                1,
		MemberID:               memberID,
		MembershipTypeID:       mt.ID,
		TypeNameSnapshot:       mt.Name,
		PriceSnapshot:          mt.Price,
		DurationDaysSnapshot:   mt.DurationDays,
		DurationMonthsSnapshot: cloneIntPtr(mt.DurationMonths),
		StartDate:              start,
		ExpiryDate:             nil,
		Status:                 StatusPendingPayment,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
}

// Activate transitions a pending_payment Membership to active in-place. Llamado
// por RegisterMembershipPayment / CreateMember-con-pago cuando llega el primer
// abono (parcial o completo — un abono parcial ya cuenta para activar).
//
// Regla de start_date: si el StartDate original quedó en el pasado (operador
// inscribió hace días, socio finalmente paga hoy), shifteamos a paymentDate
// para que el socio no pierda días de vigencia. Si el StartDate original es
// futuro (caso raro: operador programó "empieza el lunes"), lo respetamos.
//
// No-op si la membership ya está activa — devuelve el StartDate / ExpiryDate
// actuales sin error para que llamadas idempotentes (un segundo abono al
// mismo pending_payment, ya activado en la primera) no fallen.
func (m *Membership) Activate(paymentDate, now time.Time) error {
	if m.Status == StatusActive {
		return nil
	}
	if m.Status != StatusPendingPayment {
		return memErrors.ErrAdjustmentInvalidDays
	}
	pay := truncateDate(paymentDate)
	start := m.StartDate
	if start.Before(pay) {
		start = pay
	}
	expiry := computeExpiry(start, m.DurationDaysSnapshot, m.DurationMonthsSnapshot)
	m.StartDate = start
	m.ExpiryDate = &expiry
	m.Status = StatusActive
	m.Version++
	m.UpdatedAt = now
	return nil
}

// Renew computes the next Membership instance from `m` according to UC-018:
//
//	if m.ExpiryDate >= paymentDate -> new.expiry = m.expiry + new.duration_days
//	if m.ExpiryDate <  paymentDate -> new.expiry = paymentDate + new.duration_days
//
// It DOES NOT mutate `m`; it returns the new Membership and leaves the old
// one to the caller (which transitions `m.Status -> replaced` + sets
// ReplacedBy). The new Membership uses `nextType` snapshots — that's normally
// the same plan but the operator may have switched plans at renewal.
//
// Caller MUST NOT invoke Renew on a pending_payment Membership — esa
// transición es Activate(), no crear una nueva fila.
func (m *Membership) Renew(newID uuid.UUID, nextType *mtDomain.MembershipType, paymentDate time.Time, now time.Time) *Membership {
	pay := truncateDate(paymentDate)
	var newStart time.Time
	// Defensive: si por alguna razón ExpiryDate es nil cuando llega acá,
	// caemos al comportamiento "vencido" (start = payment date).
	if m.ExpiryDate != nil && !m.ExpiryDate.Before(pay) {
		// Not expired — accumulate from current expiry.
		newStart = *m.ExpiryDate
	} else {
		newStart = pay
	}
	newExpiry := computeExpiry(newStart, nextType.DurationDays, nextType.DurationMonths)
	return &Membership{
		ID:                     newID,
		GymID:                  m.GymID,
		Version:                1,
		MemberID:               m.MemberID,
		MembershipTypeID:       nextType.ID,
		TypeNameSnapshot:       nextType.Name,
		PriceSnapshot:          nextType.Price,
		DurationDaysSnapshot:   nextType.DurationDays,
		DurationMonthsSnapshot: cloneIntPtr(nextType.DurationMonths),
		StartDate:              newStart,
		ExpiryDate:             &newExpiry,
		Status:                 StatusActive,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
}

// MarkReplaced sets status=replaced and ReplacedBy. Called on the OLD
// membership when a new one supersedes it.
func (m *Membership) MarkReplaced(replacement uuid.UUID, now time.Time) {
	m.Status = StatusReplaced
	m.ReplacedBy = &replacement
	m.Version++
	m.UpdatedAt = now
}

// Cancel sets status=cancelled. Used when the operator manually voids a
// membership (rare; UC-022 refunds may trigger this).
func (m *Membership) Cancel(now time.Time) {
	m.Status = StatusCancelled
	m.Version++
	m.UpdatedAt = now
}

// AdjustExpiry mutates the expiry date and bumps version. Reason validation
// happens at the caller (use case) since it pairs with a row in
// `membership_adjustments`. UC-017: courtesy days, freezes, hand-corrections.
//
// daysAdded may be negative (a future "remove courtesy" flow). The result
// must respect chk_memberships_dates: expiry >= start.
//
// Devuelve ErrAdjustmentInvalidDays si la membership está en pending_payment
// (no tiene expiry para ajustar todavía).
func (m *Membership) AdjustExpiry(daysAdded int, now time.Time) (previousExpiry time.Time, newExpiry time.Time, err error) {
	if m.ExpiryDate == nil {
		return time.Time{}, time.Time{}, memErrors.ErrAdjustmentInvalidDays
	}
	prev := *m.ExpiryDate
	next := prev.AddDate(0, 0, daysAdded)
	if next.Before(m.StartDate) {
		return prev, prev, memErrors.ErrAdjustmentInvalidDays
	}
	m.ExpiryDate = &next
	m.Version++
	m.UpdatedAt = now
	return prev, next, nil
}

// SetExpiry sets the expiry to a specific date (UC-017 — "Establecer fecha").
// The caller passes a date already validated by the use case.
func (m *Membership) SetExpiry(target time.Time, now time.Time) (previousExpiry time.Time, daysAdded int, err error) {
	if m.ExpiryDate == nil {
		return time.Time{}, 0, memErrors.ErrAdjustmentInvalidDays
	}
	target = truncateDate(target)
	prev := *m.ExpiryDate
	if target.Before(m.StartDate) {
		return prev, 0, memErrors.ErrAdjustmentInvalidDays
	}
	delta := int(target.Sub(prev).Hours() / 24)
	m.ExpiryDate = &target
	m.Version++
	m.UpdatedAt = now
	return prev, delta, nil
}

// IsActive is a domain convenience.
func (m *Membership) IsActive(today time.Time) bool {
	if m.Status != StatusActive || m.ExpiryDate == nil {
		return false
	}
	t := truncateDate(today)
	return !m.ExpiryDate.Before(t)
}

// IsPendingPayment reports whether the socio is inscribed but hasn't paid yet.
func (m *Membership) IsPendingPayment() bool { return m.Status == StatusPendingPayment }

// DaysUntilExpiry returns positive when expiry is in the future, negative
// when expired. Returns 0 for pending_payment (no expiry yet).
func (m *Membership) DaysUntilExpiry(today time.Time) int {
	if m.ExpiryDate == nil {
		return 0
	}
	t := truncateDate(today)
	return int(m.ExpiryDate.Sub(t).Hours() / 24)
}

func truncateDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// computeExpiry calcula la fecha de vencimiento aplicando la regla del
// dominio: si durationMonths != nil, expiry = start + N meses naturales
// (con clamp al último día del mes target si el día no existe); si nil,
// expiry = start + durationDays.
//
// Esta función centraliza la decisión "días vs meses naturales" para
// que ningún caller pueda olvidar consultar el flag. New, Activate y
// Renew TODAS pasan por acá.
func computeExpiry(start time.Time, durationDays int, durationMonths *int) time.Time {
	if durationMonths != nil && *durationMonths > 0 {
		return addMonthsNatural(start, *durationMonths)
	}
	return start.AddDate(0, 0, durationDays)
}

// addMonthsNatural suma N meses al date target. A diferencia de
// time.Time.AddDate(0, N, 0), no "desborda" si el día del mes target no
// existe — por ejemplo, 31-ene + 1 mes en Go nativo da 3-mar (porque
// febrero no tiene 31 días, Go agrega los días "que sobran"). El
// operador del gym espera que el socio que pagó el 31-ene venza el 28
// o 29 de feb (el último día del mes), no 3-mar.
//
// Pasos:
//  1. Calcular el (year, month) target avanzando N meses.
//  2. Si start.Day() existe en ese mes → usar Day() tal cual.
//  3. Si no existe → usar el último día del mes target.
//
// Ejemplos (sin DST porque trabajamos en UTC, ver truncateDate):
//
//	25-may + 1 mes → 25-jun       (caso típico que motiva esta función)
//	31-ene + 1 mes → 28-feb        (año no-bisiesto)
//	31-ene + 1 mes → 29-feb        (año bisiesto)
//	30-mar + 1 mes → 30-abr
//	31-dic + 12 mes → 31-dic año+1
func addMonthsNatural(start time.Time, months int) time.Time {
	y := start.Year()
	m := int(start.Month()) + months
	// Normalizar el "carry": si m > 12, avanzar años; si <= 0, retroceder.
	for m > 12 {
		m -= 12
		y++
	}
	for m < 1 {
		m += 12
		y--
	}
	target := time.Month(m)
	day := start.Day()
	last := daysInMonth(y, target)
	if day > last {
		day = last
	}
	return time.Date(y, target, day, 0, 0, 0, 0, time.UTC)
}

// daysInMonth devuelve cuántos días tiene (year, month). Vamos al día
// 0 del mes siguiente (que en time.Date es el último del mes actual).
func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func cloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// ---------------------------------------------------------------------------
// MembershipAdjustment entity (history of UC-017 changes)
// ---------------------------------------------------------------------------

// MembershipAdjustment is an immutable history row written every time UC-017
// runs. Lives here in domain so other BCs (reports) can reference the type.
type MembershipAdjustment struct {
	ID             uuid.UUID
	GymID          uuid.UUID
	Version        int
	MembershipID   uuid.UUID
	AdjustedBy     uuid.UUID
	Reason         string
	DaysAdded      int
	PreviousExpiry time.Time
	NewExpiry      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      *time.Time
}

// NewAdjustment validates the reason length (≥5 — chk_adjustments_reason_length)
// before constructing the row.
func NewAdjustment(id uuid.UUID, gymID, membershipID, adjustedBy uuid.UUID,
	reason string, daysAdded int, previousExpiry, newExpiry time.Time, now time.Time) (*MembershipAdjustment, error) {
	r := strings.TrimSpace(reason)
	if len(r) < 5 {
		return nil, memErrors.ErrAdjustmentReasonRequired
	}
	return &MembershipAdjustment{
		ID:             id,
		GymID:          gymID,
		Version:        1,
		MembershipID:   membershipID,
		AdjustedBy:     adjustedBy,
		Reason:         r,
		DaysAdded:      daysAdded,
		PreviousExpiry: truncateDate(previousExpiry),
		NewExpiry:      truncateDate(newExpiry),
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}
