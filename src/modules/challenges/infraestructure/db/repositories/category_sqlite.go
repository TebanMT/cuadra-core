//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	categoryDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/category"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CategorySQLiteRepository struct{}

func NewCategorySQLiteRepository() *CategorySQLiteRepository {
	return &CategorySQLiteRepository{}
}

type sqliteCategoryRow struct {
	ID          string        `db:"id"`
	GymID       string        `db:"gym_id"`
	ChallengeID string        `db:"challenge_id"`
	Version     int           `db:"version"`
	CreatedAt   int64         `db:"created_at"`
	UpdatedAt   int64         `db:"updated_at"`
	DeletedAt   sql.NullInt64 `db:"deleted_at"`
	SyncedAt    sql.NullInt64 `db:"synced_at"`
	Name        string        `db:"name"`
	SortOrder   int           `db:"sort_order"`
}

func (r *CategorySQLiteRepository) Create(tx sharedDomain.Transaction, c *categoryDomain.Category) (*categoryDomain.Category, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := categoryToRow(c)
	const stmt = `
		INSERT INTO challenge_categories (
		    id, gym_id, challenge_id, version, created_at, updated_at, deleted_at,
		    name, sort_order
		) VALUES (
		    :id, :gym_id, :challenge_id, :version, :created_at, :updated_at, :deleted_at,
		    :name, :sort_order
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueCategory(stx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *CategorySQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*categoryDomain.Category, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteCategoryRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM challenge_categories WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrCategoryNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return categoryFromRow(&row), nil
}

func (r *CategorySQLiteRepository) Update(tx sharedDomain.Transaction, c *categoryDomain.Category) (*categoryDomain.Category, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := categoryToRow(c)
	const stmt = `
		UPDATE challenge_categories SET
		    version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
		    name = :name, sort_order = :sort_order
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueCategory(stx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *CategorySQLiteRepository) SoftDelete(tx sharedDomain.Transaction, id uuid.UUID) error {
	stx := tx.(*sharedDomain.SqlxTransaction)
	// Fetch first so we can enqueue a sync row with the new tombstone state.
	c, err := r.GetByID(tx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	c.DeletedAt = &now
	c.UpdatedAt = now
	c.Version++
	if _, err := stx.Exec(context.Background(),
		`UPDATE challenge_categories
		 SET deleted_at = ?, updated_at = ?, version = ?
		 WHERE id = ?`, now.UnixMilli(), now.UnixMilli(), c.Version, id.String()); err != nil {
		return err
	}
	return enqueueCategory(stx, c)
}

func (r *CategorySQLiteRepository) ListByChallenge(tx sharedDomain.Transaction, challengeID uuid.UUID) ([]*categoryDomain.Category, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteCategoryRow
	err := stx.Select(context.Background(), &rows,
		`SELECT * FROM challenge_categories
		 WHERE challenge_id = ? AND deleted_at IS NULL
		 ORDER BY sort_order ASC, created_at ASC`, challengeID.String())
	if err != nil {
		return nil, err
	}
	out := make([]*categoryDomain.Category, len(rows))
	for i := range rows {
		out[i] = categoryFromRow(&rows[i])
	}
	return out, nil
}

func (r *CategorySQLiteRepository) CountParticipants(tx sharedDomain.Transaction, categoryID uuid.UUID) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM challenge_participants
		 WHERE category_id = ? AND deleted_at IS NULL`, categoryID.String())
	return n, err
}

// ─── mappers ───────────────────────────────────────────────────────────────

func categoryToRow(c *categoryDomain.Category) sqliteCategoryRow {
	row := sqliteCategoryRow{
		ID:          c.ID.String(),
		GymID:       c.GymID.String(),
		ChallengeID: c.ChallengeID.String(),
		Version:     c.Version,
		CreatedAt:   c.CreatedAt.UnixMilli(),
		UpdatedAt:   c.UpdatedAt.UnixMilli(),
		Name:        c.Name,
		SortOrder:   c.SortOrder,
	}
	if c.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: c.DeletedAt.UnixMilli(), Valid: true}
	}
	return row
}

func categoryFromRow(r *sqliteCategoryRow) *categoryDomain.Category {
	id, _ := uuid.Parse(r.ID)
	gid, _ := uuid.Parse(r.GymID)
	cid, _ := uuid.Parse(r.ChallengeID)
	c := &categoryDomain.Category{
		ID:          id,
		GymID:       gid,
		ChallengeID: cid,
		Version:     r.Version,
		CreatedAt:   time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:   time.UnixMilli(r.UpdatedAt).UTC(),
		Name:        r.Name,
		SortOrder:   r.SortOrder,
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		c.DeletedAt = &t
	}
	return c
}

func enqueueCategory(stx *sharedDomain.SqlxTransaction, c *categoryDomain.Category) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":           c.ID.String(),
		"gym_id":       c.GymID.String(),
		"challenge_id": c.ChallengeID.String(),
		"version":      c.Version,
		"created_at":   c.CreatedAt.UnixMilli(),
		"updated_at":   c.UpdatedAt.UnixMilli(),
		"deleted_at":   nullableUnixMs(c.DeletedAt),
		"name":         c.Name,
		"sort_order":   c.SortOrder,
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "challenge_categories", c.ID.String(), "upsert", payload, c.Version)
}
