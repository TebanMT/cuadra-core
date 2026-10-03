// Package cashclose owns the physical-cash session aggregate. The persisted
// table keeps its historical name (`cash_close_events`) for wire and migration
// compatibility, but every live row represents one drawer session rather than
// an accounting result or an immutable one-per-calendar-day report.
package cashclose

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	StatusOpen             = "open"
	StatusClosedUnverified = "closed_unverified"
	StatusReconciled       = "reconciled"
	StatusStale            = "stale"
	StatusWithdrawn        = "withdrawn"

	DefaultDrawerCode        = "main"
	DestinationGymFund       = "gym_fund"
	maxMoneyCents      int64 = 999_999_999_999
)

var (
	ErrInvalidMoney          = errors.New("el monto debe ser válido y expresarse en centavos")
	ErrNegativeOpeningCash   = errors.New("el efectivo inicial no puede ser negativo")
	ErrNegativeCountedCash   = errors.New("el conteo físico no puede ser negativo")
	ErrNegativeCashLeft      = errors.New("el efectivo dejado no puede ser negativo")
	ErrCashLeftRequired      = errors.New("indica cuánto efectivo se dejará en caja")
	ErrCashLeftExceedsCount  = errors.New("el efectivo dejado no puede superar el conteo físico")
	ErrInvalidSequence       = errors.New("la secuencia de caja debe ser mayor a cero")
	ErrInvalidSessionState   = errors.New("la sesión de caja no permite esta operación")
	ErrCountRequired         = errors.New("se requiere un conteo físico para conciliar")
	ErrDifferenceReason      = errors.New("explica la diferencia del conteo")
	ErrCorrectionReason      = errors.New("explica por qué se corrige el corte")
	ErrCorrectionOwner       = errors.New("sólo el propietario puede corregir un conteo conciliado")
	ErrWithdrawalDestination = errors.New("el destino del retiro no es válido")
)

// Stable namespace: two offline devices derive the same UUID for the same
// natural session slot. Do not change it after release.
var sessionNamespace = uuid.MustParse("90e23b55-6152-5b65-8d58-8f0d64b53f60")

// CashCloseEvent is a backwards-compatible name for the CashSession
// aggregate. CloseDate and CalculatedCash remain aliases on the wire:
// CloseDate == OperationalDate and CalculatedCash == ExpectedCash.
type CashCloseEvent struct {
	ID      uuid.UUID
	GymID   uuid.UUID
	Version int

	DrawerID        uuid.UUID
	DrawerCode      string
	OperationalDate time.Time
	Sequence        int
	Status          string

	OpeningCash float64
	// OpeningCashKnown is false only for migrated legacy cuts whose former
	// schema never recorded a starting float. Such sessions remain visible but
	// cannot claim a fully trustworthy physical reconciliation.
	OpeningCashKnown bool
	ActivityCash     float64
	// CalculatedCash is the legacy storage/wire name for expected cash.
	CalculatedCash float64
	CountedCash    *float64
	CashLeft       *float64
	WithdrawnCash  *float64

	WithdrawalDestination   *string
	DiscrepancyReason       *string
	CorrectionReason        *string
	AdjustedAfterWithdrawal bool
	IntegrityNote           *string

	OpenedAt     time.Time
	OpenedBy     uuid.UUID
	ClosedAt     *time.Time
	ClosedBy     uuid.UUID // kept non-null for legacy schema; meaningful once closed
	ReconciledAt *time.Time
	ReconciledBy *uuid.UUID
	StaleAt      *time.Time
	WithdrawnAt  *time.Time
	WithdrawnBy  *uuid.UUID

	// CloseDate remains for compatibility with older clients and exports.
	CloseDate time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// DefaultDrawerID intentionally equals the gym UUID. IDs live in different
// tables/namespaces, and this gives every device a deterministic `main`
// drawer without a bootstrap round-trip.
func DefaultDrawerID(gymID uuid.UUID) uuid.UUID { return gymID }

func DrawerCode(gymID, drawerID uuid.UUID) string {
	if drawerID == uuid.Nil || drawerID == DefaultDrawerID(gymID) {
		return DefaultDrawerCode
	}
	compact := strings.ReplaceAll(drawerID.String(), "-", "")
	if len(compact) > 8 {
		compact = compact[:8]
	}
	return "drawer-" + compact
}

// DeterministicSessionID converges concurrent offline creation of a natural
// slot. sequence starts at 1 and advances after a withdrawal/turn change.
func DeterministicSessionID(gymID, drawerID uuid.UUID, operationalDate time.Time, sequence int) uuid.UUID {
	key := fmt.Sprintf("%s|%s|%s|%d", gymID, drawerID, day(operationalDate).Format("2006-01-02"), sequence)
	return uuid.NewSHA1(sessionNamespace, []byte(key))
}

// Open starts a physical drawer session. Opening cash is a stock measured at
// the beginning of the session; it is never inferred from prior counts.
func Open(gymID, drawerID, openedBy uuid.UUID, operationalDate time.Time, sequence int,
	openingCash float64, openedAt time.Time) (*CashCloseEvent, error) {
	if drawerID == uuid.Nil {
		drawerID = DefaultDrawerID(gymID)
	}
	if sequence < 1 {
		return nil, ErrInvalidSequence
	}
	opening, err := nonNegativeMoney(openingCash, ErrNegativeOpeningCash)
	if err != nil {
		return nil, err
	}
	openedAt = openedAt.UTC()
	opDate := day(operationalDate)
	return &CashCloseEvent{
		ID:               DeterministicSessionID(gymID, drawerID, opDate, sequence),
		GymID:            gymID,
		Version:          1,
		DrawerID:         drawerID,
		DrawerCode:       DrawerCode(gymID, drawerID),
		OperationalDate:  opDate,
		Sequence:         sequence,
		Status:           StatusOpen,
		OpeningCash:      opening,
		OpeningCashKnown: true,
		CalculatedCash:   opening,
		OpenedAt:         openedAt,
		OpenedBy:         openedBy,
		ClosedBy:         openedBy, // legacy NOT NULL; ClosedAt is authoritative
		CloseDate:        opDate,
		CreatedAt:        openedAt,
		UpdatedAt:        openedAt,
	}, nil
}

// New preserves the legacy constructor used by the current close use case.
// New code should call Open, Close and Reconcile so validation errors are not
// discarded. The compatibility path normalizes valid monetary inputs.
func New(id, gymID, closedBy uuid.UUID, closeDate time.Time,
	calculatedCash float64, countedCash *float64, reason *string, now time.Time) *CashCloseEvent {
	e, err := Open(gymID, DefaultDrawerID(gymID), closedBy, closeDate, 1, 0, day(closeDate))
	if err != nil {
		return nil
	}
	if id != uuid.Nil {
		e.ID = id
	}
	_ = e.Close(calculatedCash, closedBy, now)
	if countedCash != nil {
		_ = e.Reconcile(*countedCash, reason, closedBy, now)
	}
	return e
}

// Close freezes expected cash for the session. activityCash is the signed
// physical flow assigned to this drawer/session; expected = opening + activity.
func (e *CashCloseEvent) Close(activityCash float64, actor uuid.UUID, now time.Time) error {
	if e == nil || (e.Status != StatusOpen && e.Status != StatusStale) || e.DeletedAt != nil || e.FinishedAt() != nil {
		return ErrInvalidSessionState
	}
	activity, err := money(activityCash)
	if err != nil {
		return err
	}
	expected, err := money(e.OpeningCash + activity)
	if err != nil {
		return err
	}
	now = now.UTC()
	e.ActivityCash = activity
	e.CalculatedCash = expected
	e.CountedCash = nil
	e.CashLeft = nil
	e.WithdrawnCash = nil
	e.WithdrawalDestination = nil
	e.DiscrepancyReason = nil
	e.ReconciledAt = nil
	e.ReconciledBy = nil
	e.StaleAt = nil
	e.WithdrawnAt = nil
	e.WithdrawnBy = nil
	e.ClosedAt = &now
	e.ClosedBy = actor
	e.Status = StatusClosedUnverified
	e.bump(now)
	return nil
}

// Reconcile records a physical count. Difference is derived only while the
// snapshot remains reconciled/withdrawn; stale sessions deliberately expose
// no variance because comparing a new flow with an old count is misleading.
func (e *CashCloseEvent) Reconcile(countedCash float64, reason *string, actor uuid.UUID, now time.Time) error {
	if e == nil || e.DeletedAt != nil || e.FinishedAt() != nil || (e.Status != StatusClosedUnverified && e.Status != StatusReconciled) {
		return ErrInvalidSessionState
	}
	counted, err := nonNegativeMoney(countedCash, ErrNegativeCountedCash)
	if err != nil {
		return err
	}
	reason = clean(reason)
	diff, _ := money(counted - e.CalculatedCash)
	if cents(diff) != 0 && reason == nil {
		return ErrDifferenceReason
	}
	now = now.UTC()
	e.CountedCash = &counted
	e.DiscrepancyReason = reason
	e.Status = StatusReconciled
	e.ReconciledAt = &now
	e.ReconciledBy = &actor
	e.StaleAt = nil
	e.bump(now)
	return nil
}

// CorrectReconciliation replaces a still-in-drawer certified count. It is a
// correction, not the ordinary first count, and therefore preserves its own
// reason separately from a legitimate discrepancy explanation.
func (e *CashCloseEvent) CorrectReconciliation(countedCash float64, discrepancyReason *string,
	correctionReason string, actor uuid.UUID, now time.Time) error {
	if e == nil || e.Status != StatusReconciled || e.DeletedAt != nil {
		return ErrInvalidSessionState
	}
	correctionReason = strings.TrimSpace(correctionReason)
	if correctionReason == "" {
		return ErrCorrectionReason
	}
	if err := e.Reconcile(countedCash, discrepancyReason, actor, now); err != nil {
		return err
	}
	e.CorrectionReason = &correctionReason
	return nil
}

// MarkStale invalidates the variance without deleting the previous snapshot.
// The old expected/count remain available to audit; callers must refresh the
// physical activity and reconcile again before withdrawal.
func (e *CashCloseEvent) MarkStale(reason string, now time.Time) error {
	if e == nil || e.DeletedAt != nil || e.Status == StatusOpen || e.Status == StatusWithdrawn {
		return ErrInvalidSessionState
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrCorrectionReason
	}
	now = now.UTC()
	e.Status = StatusStale
	e.CorrectionReason = &reason
	e.StaleAt = &now
	e.bump(now)
	return nil
}

// Refresh replaces the frozen expected snapshot of a stale session after late
// physical activity. The prior values are preserved by the audit entry made
// by the application service; the aggregate itself never tombstones the row.
func (e *CashCloseEvent) Refresh(activityCash float64, reason string, actor uuid.UUID, now time.Time) error {
	if e == nil || e.Status != StatusStale || e.DeletedAt != nil {
		return ErrInvalidSessionState
	}
	if strings.TrimSpace(reason) == "" {
		return ErrCorrectionReason
	}
	return e.Close(activityCash, actor, now)
}

// Finish ends a counted period without moving money. CashLeft records the
// amount handed over in the drawer; WithdrawnCash/At remain nil. These existing
// wire fields distinguish a completed count from a legacy provisional count.
func (e *CashCloseEvent) Finish(now time.Time) error {
	if e == nil || e.DeletedAt != nil || e.Status != StatusReconciled || e.CountedCash == nil || !e.OpeningCashKnown {
		return ErrInvalidSessionState
	}
	if e.FinishedAt() != nil {
		return nil
	}
	left := *e.CountedCash
	e.CashLeft = &left
	e.bump(now)
	return nil
}

// FinishedAt is the inclusive boundary of an immutable physical count. A stale
// historical correction retains the boundary and the handed-over cash.
func (e *CashCloseEvent) FinishedAt() *time.Time {
	if e == nil {
		return nil
	}
	if e.WithdrawnAt != nil {
		return e.WithdrawnAt
	}
	if e.CashLeft != nil && e.CountedCash != nil {
		return e.ReconciledAt
	}
	return nil
}

// Withdraw is a distinct transition. It records a transfer from the physical
// drawer to the gym fund; it is neither income nor an expense.
func (e *CashCloseEvent) Withdraw(cashLeft float64, destination string, actor uuid.UUID, now time.Time) error {
	if e == nil || e.Status != StatusReconciled || e.CountedCash == nil || e.DeletedAt != nil {
		return ErrInvalidSessionState
	}
	left, err := nonNegativeMoney(cashLeft, ErrNegativeCashLeft)
	if err != nil {
		return err
	}
	if cents(left) > cents(*e.CountedCash) {
		return ErrCashLeftExceedsCount
	}
	destination = strings.TrimSpace(destination)
	if destination == "" {
		destination = DestinationGymFund
	}
	if destination != DestinationGymFund {
		return ErrWithdrawalDestination
	}
	withdrawn, _ := money(*e.CountedCash - left)
	now = now.UTC()
	e.CashLeft = &left
	e.WithdrawnCash = &withdrawn
	e.WithdrawalDestination = &destination
	e.WithdrawnAt = &now
	e.WithdrawnBy = &actor
	e.Status = StatusWithdrawn
	e.bump(now)
	return nil
}

// FlagAdjustedAfterWithdrawal records that an administrative correction
// changed the historical cash basis after money had already left the drawer.
// The delivered transfer remains true; the old variance is no longer
// certifiable and must not be presented as Cuadra/Sobrante/Faltante.
func (e *CashCloseEvent) FlagAdjustedAfterWithdrawal(reason string, now time.Time) error {
	if e == nil || e.Status != StatusWithdrawn || e.DeletedAt != nil {
		return ErrInvalidSessionState
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrCorrectionReason
	}
	e.AdjustedAfterWithdrawal = true
	e.IntegrityNote = &reason
	e.bump(now.UTC())
	return nil
}

// Reopen is retained for source compatibility. It now marks the same session
// stale instead of deleting it; corrections therefore keep one natural ID.
func (e *CashCloseEvent) Reopen(now time.Time) {
	_ = e.MarkStale("reapertura_manual", now)
}

func (e *CashCloseEvent) Difference() *float64 {
	if e == nil || e.CountedCash == nil || e.AdjustedAfterWithdrawal ||
		(e.Status != StatusReconciled && e.Status != StatusWithdrawn) {
		return nil
	}
	v, err := money(*e.CountedCash - e.CalculatedCash)
	if err != nil {
		return nil
	}
	return &v
}

// EffectiveStatus lets a read model surface staleness derived from current
// activity without causing a write during a report transaction.
func (e *CashCloseEvent) EffectiveStatus(hasLaterPhysicalActivity bool) string {
	if e == nil {
		return ""
	}
	if hasLaterPhysicalActivity && e.Status != StatusOpen && e.Status != StatusWithdrawn {
		return StatusStale
	}
	return e.Status
}

func (e *CashCloseEvent) ExpectedCash() float64 { return e.CalculatedCash }

func (e *CashCloseEvent) bump(now time.Time) {
	e.Version++
	e.UpdatedAt = now.UTC()
}

func clean(v *string) *string {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(*v)
	if s == "" {
		return nil
	}
	return &s
}

func nonNegativeMoney(v float64, negativeErr error) (float64, error) {
	n, err := money(v)
	if err != nil {
		return 0, err
	}
	if cents(n) < 0 {
		return 0, negativeErr
	}
	return n, nil
}

func money(v float64) (float64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, ErrInvalidMoney
	}
	scaled := v * 100
	rounded := math.Round(scaled)
	if math.Abs(scaled-rounded) > 0.000001 || math.Abs(rounded) > float64(maxMoneyCents) {
		return 0, ErrInvalidMoney
	}
	return rounded / 100, nil
}

func cents(v float64) int64 { return int64(math.Round(v * 100)) }

func day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
