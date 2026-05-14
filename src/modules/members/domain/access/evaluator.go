// Package access exposes AccessStatusEvaluator — a pure domain service used
// by the `checkins` BC (Sesión 5) to decide whether a member may enter the
// gym. Lives in `members` because the rules depend on Member.Status +
// Membership.ExpiryDate, both owned here.
package access

import (
	"time"

	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
)

// AccessStatus mirrors the chk_checkins_result enum minus `allowed_override`
// (which is a checkin-time decision, not a member state).
type AccessStatus string

const (
	AllowedActive          AccessStatus = "allowed_active"
	AllowedExpiringSoon    AccessStatus = "allowed_expiring_soon"
	DeniedExpired          AccessStatus = "denied_expired"
	DeniedInactive         AccessStatus = "denied_inactive"
	DeniedNoMembership     AccessStatus = "denied_no_membership"
	DeniedUnpaidEnrollment AccessStatus = "denied_unpaid_enrollment"
)

// ExpiringSoonThresholdDays — UC-014 / UC-034 use 7 días as "por vencer".
const ExpiringSoonThresholdDays = 7

// AccessStatusEvaluator decides the AccessStatus for (member, currentMembership)
// at `today`. It is a pure function — no I/O — so callers (checkins use cases,
// dashboard read models) can use it freely.
type AccessStatusEvaluator struct{}

// New constructs an evaluator. Stateless, but kept as a struct for parity with
// other domain services and to allow future config (e.g. configurable threshold).
func New() *AccessStatusEvaluator { return &AccessStatusEvaluator{} }

// Evaluate is the single public method. The membership argument may be nil
// (member exists but has no current membership) — the evaluator handles it.
func (e *AccessStatusEvaluator) Evaluate(m *memberDomain.Member, current *membershipDomain.Membership, today time.Time) AccessStatus {
	if m == nil {
		return DeniedNoMembership
	}
	if m.Status != memberDomain.StatusActive {
		return DeniedInactive
	}
	if current == nil {
		return DeniedNoMembership
	}
	// Pending_payment es un estado distinto a "no membership" — el socio
	// SÍ tiene un plan elegido, sólo le falta el primer abono. El kiosko
	// muestra "Falta primer pago" en lugar de "Sin membresía registrada".
	if current.Status == membershipDomain.StatusPendingPayment {
		return DeniedUnpaidEnrollment
	}
	if current.Status != membershipDomain.StatusActive || current.ExpiryDate == nil {
		return DeniedNoMembership
	}
	days := current.DaysUntilExpiry(today)
	switch {
	case days < 0:
		return DeniedExpired
	case days <= ExpiringSoonThresholdDays:
		return AllowedExpiringSoon
	default:
		return AllowedActive
	}
}
