package app

import (
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ExpiryCandidate is a row consumed by EnqueueExpiryReminder. The reader
// pulls this projection straight from the join (members + memberships +
// gyms) so the use case stays decoupled from the storage shape.
type ExpiryCandidate struct {
	GymID   uuid.UUID
	GymName string
	// GymTimezone — IANA tz del gym ("America/Mexico_City"). La usa el
	// use case para agendar el envío dentro de la ventana 8AM–9PM LOCAL.
	GymTimezone    string
	MemberID       uuid.UUID
	MemberFullName string
	MemberPhone    string
	MembershipType string
	ExpiryDate     time.Time
}

// ExpiryReader is the cross-context query surface used by UC-038. It lives
// in the notifications BC because nothing else needs it; the implementations
// (expiry_reader_postgres/sqlite.go) join members + memberships + gyms.
type ExpiryReader interface {
	// FindDueForStage returns active members whose CURRENT membership has
	// expiry_date == (hoy local del gym) - offsetDays, donde "hoy" se
	// evalúa en la zona horaria de CADA gym — no en UTC: en CDMX la
	// medianoche UTC cae a las 6 PM y la etapa se adelantaba para los
	// gyms de la tarde-noche. The use case calls it once per stage:
	// offset -3 (vence en 3 días), 0 (vence hoy), +5 (persecución post-
	// vencimiento, sólo miembros que no han renovado).
	FindDueForStage(tx sharedDomain.Transaction, now time.Time, offsetDays int) ([]ExpiryCandidate, error)
}
