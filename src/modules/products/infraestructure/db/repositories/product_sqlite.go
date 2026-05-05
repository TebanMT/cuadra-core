//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	productDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// SQLite stores money in cents (ADR-002 §2). Convert at the edge.
func toCents(v float64) int64   { return int64(math.Round(v * 100)) }
func fromCents(c int64) float64 { return float64(c) / 100 }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type ProductSQLiteRepository struct{}

func NewProductSQLiteRepository() *ProductSQLiteRepository { return &ProductSQLiteRepository{} }

type sqliteProductRow struct {
	ID           string         `db:"id"`
	GymID        string         `db:"gym_id"`
	Version      int            `db:"version"`
	CreatedAt    int64          `db:"created_at"`
	UpdatedAt    int64          `db:"updated_at"`
	DeletedAt    sql.NullInt64  `db:"deleted_at"`
	SyncedAt     sql.NullInt64  `db:"synced_at"`
	Name         string         `db:"name"`
	Price        int64          `db:"price"`
	Stock        int            `db:"stock"`
	StockMinimum int            `db:"stock_minimum"`
	Category     sql.NullString `db:"category"`
	ImageURL     sql.NullString `db:"image_url"`
	Active       int            `db:"active"`
}

func (r *ProductSQLiteRepository) Create(tx sharedDomain.Transaction, p *productDomain.Product) (*productDomain.Product, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := productToRow(p)
	const stmt = `
		INSERT INTO products (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    name, price, stock, stock_minimum, category, image_url, active
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :name, :price, :stock, :stock_minimum, :category, :image_url, :active
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueProduct(stx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *ProductSQLiteRepository) Update(tx sharedDomain.Transaction, p *productDomain.Product) (*productDomain.Product, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	p.UpdatedAt = time.Now().UTC()
	row := productToRow(p)
	const stmt = `
		UPDATE products SET
		    version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
		    name = :name, price = :price, stock = :stock, stock_minimum = :stock_minimum,
		    category = :category, image_url = :image_url, active = :active
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueProduct(stx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *ProductSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*productDomain.Product, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteProductRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM products WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(prodErrors.ErrProductNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return productFromRow(&row), nil
}

func (r *ProductSQLiteRepository) ExistsByGymAndName(tx sharedDomain.Transaction, gymID uuid.UUID, name string, excludeID *uuid.UUID) (bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	q := `SELECT COUNT(1) FROM products WHERE gym_id = ? AND name = ? COLLATE NOCASE AND deleted_at IS NULL`
	args := []any{gymID.String(), strings.TrimSpace(name)}
	if excludeID != nil {
		q += ` AND id <> ?`
		args = append(args, excludeID.String())
	}
	var n int
	if err := stx.Get(context.Background(), &n, q, args...); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *ProductSQLiteRepository) List(tx sharedDomain.Transaction, q prodRepo.ListQuery) ([]*productDomain.Product, int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	page, pageSize := normalizePage(q.Page, q.PageSize)
	where := []string{"gym_id = ?", "deleted_at IS NULL"}
	args := []any{q.GymID.String()}
	if !q.IncludeInactive {
		where = append(where, "active = 1")
	}
	if q.Category != "" {
		where = append(where, "category = ?")
		args = append(args, q.Category)
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		where = append(where, "name LIKE ? COLLATE NOCASE")
		args = append(args, s+"%")
	}
	if q.LowStockOnly {
		where = append(where, "stock <= stock_minimum")
	}
	whereClause := strings.Join(where, " AND ")
	var total int
	if err := stx.Get(context.Background(), &total,
		`SELECT COUNT(*) FROM products WHERE `+whereClause, args...); err != nil {
		return nil, 0, err
	}
	q2 := fmt.Sprintf(
		`SELECT * FROM products WHERE %s ORDER BY name COLLATE NOCASE ASC LIMIT %d OFFSET %d`,
		whereClause, pageSize, (page-1)*pageSize)
	var rows []sqliteProductRow
	if err := stx.Select(context.Background(), &rows, q2, args...); err != nil {
		return nil, 0, err
	}
	out := make([]*productDomain.Product, len(rows))
	for i := range rows {
		out[i] = productFromRow(&rows[i])
	}
	return out, total, nil
}

func productToRow(p *productDomain.Product) sqliteProductRow {
	row := sqliteProductRow{
		ID:           p.ID.String(),
		GymID:        p.GymID.String(),
		Version:      p.Version,
		CreatedAt:    p.CreatedAt.UnixMilli(),
		UpdatedAt:    p.UpdatedAt.UnixMilli(),
		Name:         p.Name,
		Price:        toCents(p.Price),
		Stock:        p.Stock,
		StockMinimum: p.StockMinimum,
		Active:       boolToInt(p.Active),
	}
	if p.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: p.DeletedAt.UnixMilli(), Valid: true}
	}
	if p.Category != nil {
		row.Category = sql.NullString{String: *p.Category, Valid: true}
	}
	if p.ImageURL != nil {
		row.ImageURL = sql.NullString{String: *p.ImageURL, Valid: true}
	}
	return row
}

func productFromRow(r *sqliteProductRow) *productDomain.Product {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	p := &productDomain.Product{
		ID:           id,
		GymID:        gymID,
		Version:      r.Version,
		Name:         r.Name,
		Price:        fromCents(r.Price),
		Stock:        r.Stock,
		StockMinimum: r.StockMinimum,
		Active:       r.Active != 0,
		CreatedAt:    time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:    time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		p.DeletedAt = &t
	}
	if r.Category.Valid {
		c := r.Category.String
		p.Category = &c
	}
	if r.ImageURL.Valid {
		u := r.ImageURL.String
		p.ImageURL = &u
	}
	return p
}

func enqueueProduct(stx *sharedDomain.SqlxTransaction, p *productDomain.Product) error {
	if stx.Queue == nil {
		return nil
	}
	// All NOT NULL columns must be in the payload — the cloud projector's
	// UPSERT only emits columns present in the map, and a missing required
	// column on first-sight INSERT triggers a 23502 NOT NULL violation.
	payload, err := json.Marshal(map[string]any{
		"id":            p.ID.String(),
		"gym_id":        p.GymID.String(),
		"version":       p.Version,
		"created_at":    p.CreatedAt.UnixMilli(),
		"updated_at":    p.UpdatedAt.UnixMilli(),
		"name":          p.Name,
		"price":         p.Price,
		"stock":         p.Stock,
		"stock_minimum": p.StockMinimum,
		"category":      strPtrOrNil(p.Category),
		"image_url":     strPtrOrNil(p.ImageURL),
		"active":        p.Active,
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "products", p.ID.String(), "upsert", payload, p.Version)
}

// strPtrOrNil returns the dereferenced string for non-nil pointers and
// untyped nil otherwise. Using nil instead of "" for nullable Postgres
// columns avoids relying on the projector's empty-string nullification.
func strPtrOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// uuidPtrOrNil returns the UUID's string form for non-nil pointers and
// untyped nil otherwise — same rationale as strPtrOrNil for FK columns.
func uuidPtrOrNil(u *uuid.UUID) any {
	if u == nil {
		return nil
	}
	return u.String()
}
