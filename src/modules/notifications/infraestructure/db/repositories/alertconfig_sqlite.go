//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	alertDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/alertconfig"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type AlertConfigSQLiteRepository struct{}

func NewAlertConfigSQLiteRepository() *AlertConfigSQLiteRepository {
	return &AlertConfigSQLiteRepository{}
}

type sqliteAlertConfigRow struct {
	GymID     string        `db:"gym_id"`
	AlertKey  string        `db:"alert_key"`
	Enabled   int           `db:"enabled"`
	Version   int           `db:"version"`
	UpdatedAt int64         `db:"updated_at"`
	DeletedAt sql.NullInt64 `db:"deleted_at"`
	SyncedAt  sql.NullInt64 `db:"synced_at"`
}

func (r *AlertConfigSQLiteRepository) Upsert(tx sharedDomain.Transaction, c *alertDomain.Config) (*alertDomain.Config, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var existing sqliteAlertConfigRow
	err := stx.Get(context.Background(), &existing,
		`SELECT * FROM owner_alert_configs WHERE gym_id = ? AND alert_key = ? AND deleted_at IS NULL`,
		c.GymID.String(), string(c.Key))
	if errors.Is(err, sql.ErrNoRows) {
		row := alertConfigToRow(c)
		const stmt = `
			INSERT INTO owner_alert_configs (
			    gym_id, alert_key, enabled, version, updated_at, deleted_at
			) VALUES (
			    :gym_id, :alert_key, :enabled, :version, :updated_at, :deleted_at
			)`
		if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
			return nil, err
		}
		if err := enqueueAlertConfig(stx, c); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	c.UpdatedAt = time.Now().UTC()
	row := alertConfigToRow(c)
	const stmt = `
		UPDATE owner_alert_configs SET
		    enabled = :enabled, version = :version, updated_at = :updated_at
		WHERE gym_id = :gym_id AND alert_key = :alert_key`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueAlertConfig(stx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *AlertConfigSQLiteRepository) GetByGymAndKey(tx sharedDomain.Transaction, gymID uuid.UUID, key alertDomain.Key) (*alertDomain.Config, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteAlertConfigRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM owner_alert_configs WHERE gym_id = ? AND alert_key = ? AND deleted_at IS NULL`,
		gymID.String(), string(key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return alertConfigFromRow(&row)
}

func (r *AlertConfigSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*alertDomain.Config, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteAlertConfigRow
	if err := stx.Select(context.Background(), &rows,
		`SELECT * FROM owner_alert_configs WHERE gym_id = ? AND deleted_at IS NULL ORDER BY alert_key ASC`,
		gymID.String()); err != nil {
		return nil, err
	}
	out := make([]*alertDomain.Config, 0, len(rows))
	for i := range rows {
		c, err := alertConfigFromRow(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func alertConfigToRow(c *alertDomain.Config) map[string]any {
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	row := map[string]any{
		"gym_id":     c.GymID.String(),
		"alert_key":  string(c.Key),
		"enabled":    enabled,
		"version":    c.Version,
		"updated_at": c.UpdatedAt.UTC().UnixMilli(),
		"deleted_at": sql.NullInt64{},
	}
	if c.DeletedAt != nil {
		row["deleted_at"] = sql.NullInt64{Int64: c.DeletedAt.UTC().UnixMilli(), Valid: true}
	}
	return row
}

func alertConfigFromRow(r *sqliteAlertConfigRow) (*alertDomain.Config, error) {
	gymID, err := uuid.Parse(r.GymID)
	if err != nil {
		return nil, err
	}
	c := &alertDomain.Config{
		GymID:     gymID,
		Key:       alertDomain.Key(r.AlertKey),
		Enabled:   r.Enabled == 1,
		Version:   r.Version,
		UpdatedAt: time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		c.DeletedAt = &t
	}
	return c, nil
}

// enqueueAlertConfig pushes the row into sync_queue so the agent forwards it
// upstream. The natural primary key (gym_id, alert_key) means the cloud
// projector has to upsert by composite key — see SyncedTables.CompositeKey
// in shared/sync/tables.go.
//
// The wire `entity_id` is a deterministic UUIDv5 derived from
// (gym_id, alert_key). That keeps the cloud `sync_entities` table happy
// (its primary key requires a UUID) while still letting the same toggle
// coalesce across pushes (the same input always produces the same UUID).
func enqueueAlertConfig(stx *sharedDomain.SqlxTransaction, c *alertDomain.Config) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"gym_id":     c.GymID.String(),
		"alert_key":  string(c.Key),
		"enabled":    c.Enabled,
		"version":    c.Version,
		"updated_at": c.UpdatedAt.UTC().UnixMilli(),
	})
	if err != nil {
		return err
	}
	entityID := alertDomain.EntityID(c.GymID, c.Key).String()
	return stx.EnqueueSync(context.Background(), "owner_alert_configs", entityID, "upsert", payload, c.Version)
}
