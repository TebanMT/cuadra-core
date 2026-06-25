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
	whereClause, args := buildProductWhereSqlite(q)
	var total int
	if err := stx.Get(context.Background(), &total,
		`SELECT COUNT(*) FROM products WHERE `+whereClause, args...); err != nil {
		return nil, 0, err
	}
	q2 := fmt.Sprintf(
		`SELECT * FROM products WHERE %s ORDER BY %s LIMIT %d OFFSET %d`,
		whereClause, sortClauseSqlite(q.Sort, q.Direction), pageSize, (page-1)*pageSize)
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

// ListAggregates corre las mismas WHERE de List pero sin paginar ni
// ordenar — solo SUM/COUNT sobre el set completo. TotalValue siempre
// se restringe a activos porque inactivos no se venden. Devuelve totales
// en moneda (cents → float al edge, mismo patrón que el resto del
// modulo).
//
// El costo unitario por producto sale de un LEFT JOIN a la subconsulta de
// stock_movements: promedio ponderado por cantidad de las entradas
// `restock` con costo unitario. (2*SUM(cost*delta) + SUM(delta)) /
// (2*SUM(delta)) con división entera = redondeo half-up a centavos (cost y
// delta son no-negativos). Coincide centavo a centavo con
// ROUND(SUM(cost*delta)/SUM(delta),2) de Postgres para que ambos binarios
// devuelvan el MISMO número. Todo el cálculo de costo/ganancia se hace en
// centavos enteros y se divide /100 al edge.
func (r *ProductSQLiteRepository) ListAggregates(tx sharedDomain.Transaction, q prodRepo.ListQuery) (prodRepo.ProductAggregates, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	whereClause, args := buildProductWhereSqlite(q)
	// Un solo round-trip — calcular todo en una query usando SUM/COUNT
	// condicionales sobre la WHERE filtrada. Más eficiente que varias
	// queries separadas, especialmente para catálogos chicos donde el
	// overhead por query domina.
	var row struct {
		TotalValueCents        sql.NullInt64 `db:"total_value"`
		LowCount               int           `db:"low_count"`
		OutCount               int           `db:"out_count"`
		CostValueCents         sql.NullInt64 `db:"cost_value"`
		PotentialProfitCents   sql.NullInt64 `db:"potential_profit"`
		SaleValueWithCostCents sql.NullInt64 `db:"sale_value_with_cost"`
		ProductsTotal          int           `db:"products_total"`
		ProductsWithCost       int           `db:"products_with_cost"`
	}
	stmt := fmt.Sprintf(`
		SELECT
		  COALESCE(SUM(CASE WHEN p.active = 1 THEN p.price * p.stock ELSE 0 END), 0) AS total_value,
		  COALESCE(SUM(CASE WHEN p.active = 1 AND p.stock > 0 AND p.stock <= p.stock_minimum THEN 1 ELSE 0 END), 0) AS low_count,
		  COALESCE(SUM(CASE WHEN p.active = 1 AND p.stock = 0 THEN 1 ELSE 0 END), 0) AS out_count,
		  COALESCE(SUM(CASE WHEN p.active = 1 AND c.avg_unit_cost IS NOT NULL THEN p.stock * c.avg_unit_cost ELSE 0 END), 0) AS cost_value,
		  COALESCE(SUM(CASE WHEN p.active = 1 AND c.avg_unit_cost IS NOT NULL THEN p.stock * (p.price - c.avg_unit_cost) ELSE 0 END), 0) AS potential_profit,
		  COALESCE(SUM(CASE WHEN p.active = 1 AND c.avg_unit_cost IS NOT NULL THEN p.stock * p.price ELSE 0 END), 0) AS sale_value_with_cost,
		  COALESCE(SUM(CASE WHEN p.active = 1 THEN 1 ELSE 0 END), 0) AS products_total,
		  COALESCE(SUM(CASE WHEN p.active = 1 AND c.avg_unit_cost IS NOT NULL THEN 1 ELSE 0 END), 0) AS products_with_cost
		FROM products p
		LEFT JOIN (%s) c ON c.product_id = p.id
		WHERE %s`, avgUnitCostSubquerySqlite, whereClause)
	// El placeholder de gym_id de la subconsulta va ANTES de los args de
	// la WHERE (aparece antes en el SQL).
	subArgs := append([]any{q.GymID.String()}, args...)
	if err := stx.Get(context.Background(), &row, stmt, subArgs...); err != nil {
		return prodRepo.ProductAggregates{}, err
	}
	return prodRepo.ProductAggregates{
		TotalValue:        float64(row.TotalValueCents.Int64) / 100,
		LowCount:          row.LowCount,
		OutCount:          row.OutCount,
		CostValue:         float64(row.CostValueCents.Int64) / 100,
		PotentialProfit:   float64(row.PotentialProfitCents.Int64) / 100,
		SaleValueWithCost: float64(row.SaleValueWithCostCents.Int64) / 100,
		ProductsTotal:     row.ProductsTotal,
		ProductsWithCost:  row.ProductsWithCost,
	}, nil
}

// avgUnitCostSubquerySqlite — costo unitario promedio ponderado por
// cantidad por producto en CENTAVOS enteros (cost y price se guardan en
// centavos en SQLite). `cost` es unitario; el ponderado es
// SUM(cost*delta)/SUM(delta) con redondeo half-up vía división entera.
// `restock` literal igual que el resto de las queries de inventario del
// repo de reportes.
const avgUnitCostSubquerySqlite = `
		  SELECT product_id, (2*SUM(cost * delta) + SUM(delta)) / (2*SUM(delta)) AS avg_unit_cost
		  FROM stock_movements
		  WHERE gym_id = ? AND movement_type = 'restock' AND cost IS NOT NULL AND deleted_at IS NULL
		  GROUP BY product_id
		  HAVING SUM(delta) > 0`

// ListUnitCosts — costo unitario promedio (pesos) por producto del filtro,
// solo de los que tienen costo capturado. INNER JOIN a la subconsulta de
// costo: el mapa cubre exactamente el subconjunto con costo.
func (r *ProductSQLiteRepository) ListUnitCosts(tx sharedDomain.Transaction, q prodRepo.ListQuery) (map[uuid.UUID]float64, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	whereClause, args := buildProductWhereSqlite(q)
	stmt := fmt.Sprintf(`
		SELECT p.id AS id, c.avg_unit_cost AS avg_unit_cost
		FROM products p
		JOIN (%s) c ON c.product_id = p.id
		WHERE %s`, avgUnitCostSubquerySqlite, whereClause)
	subArgs := append([]any{q.GymID.String()}, args...)
	var rows []struct {
		ID           string `db:"id"`
		AvgUnitCostC int64  `db:"avg_unit_cost"`
	}
	if err := stx.Select(context.Background(), &rows, stmt, subArgs...); err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]float64, len(rows))
	for _, x := range rows {
		id, err := uuid.Parse(x.ID)
		if err != nil {
			continue
		}
		out[id] = float64(x.AvgUnitCostC) / 100
	}
	return out, nil
}

// buildProductWhereSqlite — extraído para reuso entre List y
// ListAggregates. Cualquier cambio de filtro (nueva columna, nuevo
// shape) entra aquí una sola vez.
func buildProductWhereSqlite(q prodRepo.ListQuery) (string, []any) {
	where := []string{"gym_id = ?", "deleted_at IS NULL"}
	args := []any{q.GymID.String()}
	// Default a "active" cuando el caller no pasa filtro — preserva el
	// comportamiento histórico de la página de productos y de
	// useActiveProducts en el FE.
	switch q.ActiveFilter {
	case prodRepo.ActiveFilterAll:
		// no filter
	case prodRepo.ActiveFilterInactive:
		where = append(where, "active = 0")
	default:
		where = append(where, "active = 1")
	}
	if q.Category != "" {
		where = append(where, "category = ?")
		args = append(args, q.Category)
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		// Substring match (%s%) — antes era prefix (s%), lo que dejaba
		// fuera resultados como "Agua mineral" al buscar "mineral". El
		// catálogo típico de un gym tiene <200 SKUs así que el full
		// scan que esto implica no es problema.
		where = append(where, "name LIKE ? COLLATE NOCASE")
		args = append(args, "%"+s+"%")
	}
	if q.LowStockOnly {
		where = append(where, "stock <= stock_minimum")
	}
	return strings.Join(where, " AND "), args
}

// sortClauseSqlite — devuelve un ORDER BY válido. Whitelist explícita:
// el valor viene del cliente vía query string y no quiero permitir
// arbitrary column injection. Cualquier valor desconocido cae a
// `name ASC` (default histórico).
func sortClauseSqlite(sort, dir string) string {
	col := "name COLLATE NOCASE"
	switch sort {
	case prodRepo.SortPrice:
		col = "price"
	case prodRepo.SortStock:
		col = "stock"
	case prodRepo.SortCategory:
		col = "category COLLATE NOCASE"
	}
	direction := "ASC"
	if dir == prodRepo.SortDirDesc {
		direction = "DESC"
	}
	// Tiebreaker por nombre para resultados estables cuando hay
	// empate en la columna elegida (p.ej. dos productos con el mismo
	// precio).
	if sort == prodRepo.SortName || sort == "" {
		return col + " " + direction
	}
	return col + " " + direction + ", name COLLATE NOCASE ASC"
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
