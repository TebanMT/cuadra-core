//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type TemplateOverrideSQLiteRepository struct{}

func NewTemplateOverrideSQLiteRepository() *TemplateOverrideSQLiteRepository {
	return &TemplateOverrideSQLiteRepository{}
}

type sqliteTemplateRow struct {
	ID          string        `db:"id"`
	GymID       string        `db:"gym_id"`
	Version     int           `db:"version"`
	CreatedAt   int64         `db:"created_at"`
	UpdatedAt   int64         `db:"updated_at"`
	DeletedAt   sql.NullInt64 `db:"deleted_at"`
	SyncedAt    sql.NullInt64 `db:"synced_at"`
	TemplateKey string        `db:"template_key"`
	Body        string        `db:"body"`
	Enabled     int           `db:"enabled"`
}

func (r *TemplateOverrideSQLiteRepository) Upsert(tx sharedDomain.Transaction, o *tplDomain.Override) (*tplDomain.Override, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var existing sqliteTemplateRow
	err := stx.Get(context.Background(), &existing,
		`SELECT * FROM notification_templates WHERE gym_id = ? AND template_key = ? AND deleted_at IS NULL`,
		o.GymID.String(), o.TemplateKey)
	if errors.Is(err, sql.ErrNoRows) {
		// insert
		row := overrideToRow(o)
		const stmt = `
			INSERT INTO notification_templates (
			    id, gym_id, version, created_at, updated_at, deleted_at,
			    template_key, body, enabled
			) VALUES (
			    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
			    :template_key, :body, :enabled
			)`
		if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
			return nil, err
		}
		if err := enqueueTemplate(stx, o); err != nil {
			return nil, err
		}
		return o, nil
	}
	if err != nil {
		return nil, err
	}
	o.ID, _ = uuid.Parse(existing.ID)
	o.CreatedAt = time.UnixMilli(existing.CreatedAt).UTC()
	o.UpdatedAt = time.Now().UTC()
	row := overrideToRow(o)
	const stmt = `
		UPDATE notification_templates SET
		    version = :version, updated_at = :updated_at,
		    body = :body, enabled = :enabled
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueTemplate(stx, o); err != nil {
		return nil, err
	}
	return o, nil
}

func (r *TemplateOverrideSQLiteRepository) GetByGymAndKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*tplDomain.Override, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteTemplateRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM notification_templates WHERE gym_id = ? AND template_key = ? AND deleted_at IS NULL`,
		gymID.String(), key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return overrideFromRow(&row)
}

func (r *TemplateOverrideSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*tplDomain.Override, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteTemplateRow
	if err := stx.Select(context.Background(), &rows,
		`SELECT * FROM notification_templates WHERE gym_id = ? AND deleted_at IS NULL ORDER BY template_key ASC`,
		gymID.String()); err != nil {
		return nil, err
	}
	out := make([]*tplDomain.Override, 0, len(rows))
	for i := range rows {
		o, err := overrideFromRow(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func overrideToRow(o *tplDomain.Override) map[string]any {
	enabled := 0
	if o.Enabled {
		enabled = 1
	}
	row := map[string]any{
		"id":           o.ID.String(),
		"gym_id":       o.GymID.String(),
		"version":      o.Version,
		"created_at":   o.CreatedAt.UTC().UnixMilli(),
		"updated_at":   o.UpdatedAt.UTC().UnixMilli(),
		"deleted_at":   sql.NullInt64{},
		"template_key": o.TemplateKey,
		"body":         o.Body,
		"enabled":      enabled,
	}
	if o.DeletedAt != nil {
		row["deleted_at"] = sql.NullInt64{Int64: o.DeletedAt.UTC().UnixMilli(), Valid: true}
	}
	return row
}

func overrideFromRow(r *sqliteTemplateRow) (*tplDomain.Override, error) {
	id, err := uuid.Parse(r.ID)
	if err != nil {
		return nil, err
	}
	gymID, err := uuid.Parse(r.GymID)
	if err != nil {
		return nil, err
	}
	o := &tplDomain.Override{
		ID:          id,
		GymID:       gymID,
		Version:     r.Version,
		TemplateKey: r.TemplateKey,
		Body:        r.Body,
		Enabled:     r.Enabled == 1,
		CreatedAt:   time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:   time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		o.DeletedAt = &t
	}
	return o, nil
}

func enqueueTemplate(stx *sharedDomain.SqlxTransaction, o *tplDomain.Override) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":           o.ID.String(),
		"gym_id":       o.GymID.String(),
		"version":      o.Version,
		"template_key": o.TemplateKey,
		"body":         o.Body,
		"enabled":      o.Enabled,
		"updated_at":   o.UpdatedAt.UTC().UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "notification_templates", o.ID.String(), "upsert", payload, o.Version)
}
