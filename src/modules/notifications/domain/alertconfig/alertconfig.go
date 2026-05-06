// Package alertconfig owns the per-gym owner-alert toggle (UC-040 DA-40.1).
//
// The five alert keys the FE settings page renders are declared here as the
// canonical, ordered list. A row in `owner_alert_configs` exists only when
// the owner has flipped the default — absence of a row means "use default"
// (today, every key defaults to enabled). The dispatcher consults this
// table before enqueueing an owner alert; templates themselves stay in the
// notifications/domain/template library.
package alertconfig

import (
	"strings"
	"time"

	"github.com/google/uuid"

	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
)

// Key is the FE-facing alert key — same string the JSON payload carries on
// the wire. Cuadra also reuses these as the OwnerAlertKind in the dispatch
// path, so the toggle and the trigger speak one vocabulary.
type Key string

const (
	KeyLowStock         Key = "low_stock"
	KeyExpiredNoContact Key = "expired_no_contact"
	KeyVIPMemberNoVisit Key = "vip_member_no_visit"
	KeyCashCloseDiff    Key = "cash_close_diff"
	KeyNoPaymentsToday  Key = "no_payments_today"
)

// Definition is the immutable metadata that backs the GET /owner-alerts
// payload. Order is stable for tests and UI.
type Definition struct {
	Key         Key
	Description string
	// EnabledByDefault is the value List() reports when no override exists.
	// Today every key defaults true; the field exists so we can ship a
	// noisy alert later as opt-in without changing the persistence shape.
	EnabledByDefault bool
}

// Defaults returns the canonical alert library. Adding a new key here is a
// code change — the migration's primary key (gym_id, alert_key) accepts
// arbitrary strings, so removing or renaming a key is also safe (stale rows
// for unknown keys are ignored on read).
func Defaults() []Definition {
	return []Definition{
		{KeyLowStock, "Avisar cuando un producto llegue al stock mínimo", true},
		{KeyExpiredNoContact, "Avisar de socios vencidos hace ≥7 días sin contacto", true},
		{KeyVIPMemberNoVisit, "Avisar cuando un socio VIP no asiste en 14+ días", true},
		{KeyCashCloseDiff, "Avisar cuando el corte de caja tiene diferencia", true},
		{KeyNoPaymentsToday, "Avisar si no se cobró nada en el día", true},
	}
}

// LookupDefault returns the Definition for a key, or nil when the key is
// not in the canonical list.
func LookupDefault(k Key) *Definition {
	for _, d := range Defaults() {
		if d.Key == k {
			d := d
			return &d
		}
	}
	return nil
}

// Config is the per-gym row in `owner_alert_configs`. Only persisted when
// the owner has explicitly set a value — see the package doc.
type Config struct {
	GymID     uuid.UUID
	Key       Key
	Enabled   bool
	Version   int
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// New builds a fresh Config. The key must exist in Defaults; the validation
// keeps stale FE state from polluting the DB with unknown keys.
func New(gymID uuid.UUID, key Key, enabled bool, now time.Time) (*Config, error) {
	k := Key(strings.TrimSpace(string(key)))
	if LookupDefault(k) == nil {
		return nil, notiErrors.ErrAlertKeyUnknown
	}
	return &Config{
		GymID:     gymID,
		Key:       k,
		Enabled:   enabled,
		Version:   1,
		UpdatedAt: now,
	}, nil
}

// SetEnabled toggles the value and bumps the version when it actually
// changes. No-op when the new value matches the old (so an idempotent
// PATCH from the FE doesn't churn the version).
func (c *Config) SetEnabled(enabled bool, now time.Time) {
	if c.Enabled == enabled {
		return
	}
	c.Enabled = enabled
	c.Version++
	c.UpdatedAt = now
}

// EntityID derives the wire-protocol entity_id for an owner-alert toggle.
// The owner_alert_configs table has no surrogate id (composite PK
// gym_id, alert_key), but the sync layer's sync_entities table needs a
// UUID. We mint a deterministic UUIDv5 from the (gym_id, alert_key) pair
// so the same toggle always coalesces to the same row across pushes.
func EntityID(gymID uuid.UUID, key Key) uuid.UUID {
	return uuid.NewSHA1(gymID, []byte(string(key)))
}
