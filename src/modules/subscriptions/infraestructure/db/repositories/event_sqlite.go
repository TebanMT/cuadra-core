//go:build sidecar

// Package repositories — implementación SQLite del repo de subscription events.
// El sidecar nunca ESCRIBE en esta tabla (los webhooks viven cloud-only); se
// limita a leerla. Las filas llegan vía el sync agent + projector genérico,
// que upserts directamente sobre la tabla. Insert() / ExistsByExternalID() se
// implementan por contrato pero no esperamos que sean llamados localmente.
package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type EventSQLiteRepository struct{}

func NewEventSQLiteRepository() *EventSQLiteRepository { return &EventSQLiteRepository{} }

type sqliteSubEventRow struct {
	ID           string          `db:"id"`
	GymID        string          `db:"gym_id"`
	Provider     string          `db:"provider"`
	Type         string          `db:"type"`
	ExternalID   string          `db:"external_id"`
	Plan         string          `db:"plan"`
	Amount       sql.NullFloat64 `db:"amount"`
	Currency     sql.NullString  `db:"currency"`
	PeriodEndsAt sql.NullInt64   `db:"period_ends_at"`
	RawPayload   string          `db:"raw_payload"`
	OccurredAt   int64           `db:"occurred_at"`
	RecordedAt   int64           `db:"recorded_at"`
}

// Insert es no-op desde el sidecar. Devuelve ErrDuplicateEvent porque ningún
// flujo legítimo del sidecar inserta acá — los eventos se proyectan vía sync.
// Si algún día queremos permitir escritura local (ej. evento "manual" desde
// CLI), reemplazar este body por un INSERT real con sync_queue dispatch.
func (r *EventSQLiteRepository) Insert(_ sharedDomain.Transaction, _ *subDomain.Event) error {
	return subDomain.ErrDuplicateEvent
}

// ExistsByExternalID — el sidecar no procesa webhooks así que no hace falta
// el chequeo de idempotencia. Devolvemos false para que cualquier caller
// inocente (no debería existir) avance a Insert que será no-op.
func (r *EventSQLiteRepository) ExistsByExternalID(_ sharedDomain.Transaction, _ subDomain.Provider, _ string) (bool, error) {
	return false, nil
}

// ListByGym devuelve los últimos `limit` eventos del gym ordenados por
// occurred_at desc. Es lo que la página Suscripción del desktop renderea
// como "historial de pagos".
func (r *EventSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]*subDomain.Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteSubEventRow
	err := stx.Select(context.Background(), &rows, `
		SELECT id, gym_id, provider, type, external_id, plan,
		       amount, currency, period_ends_at, raw_payload,
		       occurred_at, recorded_at
		FROM subscription_events
		WHERE gym_id = ? AND deleted_at IS NULL
		ORDER BY occurred_at DESC
		LIMIT ?`,
		gymID.String(), limit,
	)
	if err != nil {
		return nil, err
	}
	out := make([]*subDomain.Event, 0, len(rows))
	for _, row := range rows {
		ev, err := rowToEvent(row)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func rowToEvent(row sqliteSubEventRow) (*subDomain.Event, error) {
	id, err := uuid.Parse(row.ID)
	if err != nil {
		return nil, err
	}
	gymID, err := uuid.Parse(row.GymID)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if strings.TrimSpace(row.RawPayload) != "" {
		_ = json.Unmarshal([]byte(row.RawPayload), &raw)
	}
	var amount *float64
	if row.Amount.Valid {
		v := row.Amount.Float64
		amount = &v
	}
	var currency *string
	if row.Currency.Valid {
		v := row.Currency.String
		currency = &v
	}
	var periodEnd *time.Time
	if row.PeriodEndsAt.Valid {
		t := time.UnixMilli(row.PeriodEndsAt.Int64).UTC()
		periodEnd = &t
	}
	return &subDomain.Event{
		ID:           id,
		GymID:        gymID,
		Provider:     subDomain.Provider(row.Provider),
		Type:         subDomain.EventType(row.Type),
		ExternalID:   row.ExternalID,
		Plan:         row.Plan,
		Amount:       amount,
		Currency:     currency,
		PeriodEndsAt: periodEnd,
		RawPayload:   raw,
		OccurredAt:   time.UnixMilli(row.OccurredAt).UTC(),
		RecordedAt:   time.UnixMilli(row.RecordedAt).UTC(),
	}, nil
}
