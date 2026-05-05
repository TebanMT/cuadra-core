//go:build server

package db

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// SubscriptionEventModel is the GORM row.
type SubscriptionEventModel struct {
	ID           uuid.UUID  `gorm:"primaryKey;column:id"`
	GymID        uuid.UUID  `gorm:"not null;column:gym_id;index"`
	Provider     string     `gorm:"not null;column:provider"`
	Type         string     `gorm:"not null;column:type"`
	ExternalID   string     `gorm:"not null;column:external_id;uniqueIndex:ux_sub_event_external"`
	Plan         string     `gorm:"not null;column:plan"`
	Amount       *float64   `gorm:"column:amount"`
	Currency     *string    `gorm:"column:currency"`
	PeriodEndsAt *time.Time `gorm:"column:period_ends_at"`
	RawPayload   string     `gorm:"type:jsonb;column:raw_payload"`
	OccurredAt   time.Time  `gorm:"not null;column:occurred_at"`
	RecordedAt   time.Time  `gorm:"not null;column:recorded_at"`
}

func (SubscriptionEventModel) TableName() string { return "subscription_events" }

type EventPostgresRepository struct{}

func NewEventPostgresRepository() *EventPostgresRepository { return &EventPostgresRepository{} }

func (r *EventPostgresRepository) Insert(tx sharedDomain.Transaction, e *subDomain.Event) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	raw := "{}"
	if e.RawPayload != nil {
		if b, err := json.Marshal(e.RawPayload); err == nil {
			raw = string(b)
		}
	}
	row := SubscriptionEventModel{
		ID:           e.ID,
		GymID:        e.GymID,
		Provider:     string(e.Provider),
		Type:         string(e.Type),
		ExternalID:   e.ExternalID,
		Plan:         e.Plan,
		Amount:       e.Amount,
		Currency:     e.Currency,
		PeriodEndsAt: e.PeriodEndsAt,
		RawPayload:   raw,
		OccurredAt:   e.OccurredAt,
		RecordedAt:   e.RecordedAt,
	}
	err := gormTx.Create(&row).Error
	if err != nil {
		// Postgres unique-violation surfaces with the constraint name; the
		// repository contract converts that into a typed error.
		if isUniqueViolation(err) {
			return subDomain.ErrDuplicateEvent
		}
		return err
	}
	return nil
}

func (r *EventPostgresRepository) ExistsByExternalID(tx sharedDomain.Transaction, provider subDomain.Provider, externalID string) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&SubscriptionEventModel{}).
		Where("provider = ? AND external_id = ?", string(provider), externalID).
		Count(&n).Error
	return n > 0, err
}

func (r *EventPostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]*subDomain.Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []SubscriptionEventModel
	err := gormTx.Where("gym_id = ?", gymID).
		Order("occurred_at DESC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]*subDomain.Event, 0, len(rows))
	for i := range rows {
		out = append(out, fromModel(&rows[i]))
	}
	return out, nil
}

func fromModel(m *SubscriptionEventModel) *subDomain.Event {
	var raw map[string]any
	if m.RawPayload != "" {
		_ = json.Unmarshal([]byte(m.RawPayload), &raw)
	}
	return &subDomain.Event{
		ID:           m.ID,
		GymID:        m.GymID,
		Provider:     subDomain.Provider(m.Provider),
		Type:         subDomain.EventType(m.Type),
		ExternalID:   m.ExternalID,
		Plan:         m.Plan,
		Amount:       m.Amount,
		Currency:     m.Currency,
		PeriodEndsAt: m.PeriodEndsAt,
		RawPayload:   raw,
		OccurredAt:   m.OccurredAt,
		RecordedAt:   m.RecordedAt,
	}
}

// isUniqueViolation tests both pq's "unique_violation" SQLSTATE and the GORM
// generic error wrapping. We avoid a hard dependency on github.com/lib/pq by
// matching the message; this is fine for our single uniqueness path.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// gorm wraps duplicates as gorm.ErrDuplicatedKey on supported drivers.
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return contains(msg, "duplicate key value violates unique constraint") ||
		contains(msg, "UNIQUE constraint failed")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
