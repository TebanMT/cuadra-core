//go:build server

package repositories

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	productDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	"github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type ProductPostgresRepository struct{}

func NewProductPostgresRepository() *ProductPostgresRepository {
	return &ProductPostgresRepository{}
}

func (r *ProductPostgresRepository) Create(tx sharedDomain.Transaction, p *productDomain.Product) (*productDomain.Product, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := productToModel(p)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return productFromModel(&row), nil
}

func (r *ProductPostgresRepository) Update(tx sharedDomain.Transaction, p *productDomain.Product) (*productDomain.Product, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	p.UpdatedAt = time.Now().UTC()
	if err := gormTx.Model(&models.ProductModel{}).Where("id = ?", p.ID).
		Updates(map[string]any{
			"version":       p.Version,
			"updated_at":    p.UpdatedAt,
			"deleted_at":    p.DeletedAt,
			"name":          p.Name,
			"price":         p.Price,
			"stock":         p.Stock,
			"stock_minimum": p.StockMinimum,
			"category":      p.Category,
			"image_url":     p.ImageURL,
			"active":        p.Active,
		}).Error; err != nil {
		return nil, err
	}
	return p, nil
}

func (r *ProductPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*productDomain.Product, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.ProductModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(prodErrors.ErrProductNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return productFromModel(&row), nil
}

func (r *ProductPostgresRepository) ExistsByGymAndName(tx sharedDomain.Transaction, gymID uuid.UUID, name string, excludeID *uuid.UUID) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	q := gormTx.Model(&models.ProductModel{}).
		Where("gym_id = ? AND LOWER(name) = ? AND deleted_at IS NULL",
			gymID, strings.ToLower(strings.TrimSpace(name)))
	if excludeID != nil {
		q = q.Where("id <> ?", *excludeID)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *ProductPostgresRepository) List(tx sharedDomain.Transaction, q prodRepo.ListQuery) ([]*productDomain.Product, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	page, pageSize := normalizePage(q.Page, q.PageSize)
	base := buildProductFilterPg(gormTx.Model(&models.ProductModel{}), q)
	var total int64
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []models.ProductModel
	if err := base.Order(sortClausePostgres(q.Sort, q.Direction)).
		Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*productDomain.Product, len(rows))
	for i := range rows {
		out[i] = productFromModel(&rows[i])
	}
	return out, int(total), nil
}

// ListAggregates — paralelo a la versión SQLite. Una sola query que
// agrega total_value (solo activos), low_count, out_count y la ganancia
// potencial sobre el stock usando SUM(CASE WHEN ...). Sobre catálogos de
// 50-200 SKUs es trivial.
//
// El costo unitario por producto sale de una subconsulta LEFT JOIN sobre
// stock_movements: ROUND(SUM(cost*delta)/SUM(delta), 2) — promedio
// ponderado por cantidad de las entradas `restock` con costo unitario.
// ROUND a 2 decimales redondea a centavos half-away-from-zero; como cost
// y delta son no-negativos, coincide centavo a centavo con el redondeo
// entero de la versión SQLite ((2*SUM(cost*delta)+den)/(2*den)) — así
// ambos binarios devuelven el MISMO número.
func (r *ProductPostgresRepository) ListAggregates(tx sharedDomain.Transaction, q prodRepo.ListQuery) (prodRepo.ProductAggregates, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	base := buildProductFilterPg(gormTx.Model(&models.ProductModel{}), q)
	costSub := avgUnitCostSubqueryPg(gormTx, q.GymID)
	var row struct {
		TotalValue        float64
		LowCount          int
		OutCount          int
		CostValue         float64
		PotentialProfit   float64
		SaleValueWithCost float64
		ProductsTotal     int
		ProductsWithCost  int
	}
	if err := base.Session(&gorm.Session{}).
		Joins("LEFT JOIN (?) AS c ON c.product_id = products.id", costSub).
		Select(`
		COALESCE(SUM(CASE WHEN products.active = true THEN products.price * products.stock ELSE 0 END), 0) AS total_value,
		COALESCE(SUM(CASE WHEN products.active = true AND products.stock > 0 AND products.stock <= products.stock_minimum THEN 1 ELSE 0 END), 0) AS low_count,
		COALESCE(SUM(CASE WHEN products.active = true AND products.stock = 0 THEN 1 ELSE 0 END), 0) AS out_count,
		COALESCE(SUM(CASE WHEN products.active = true AND c.avg_unit_cost IS NOT NULL THEN products.stock * c.avg_unit_cost ELSE 0 END), 0) AS cost_value,
		COALESCE(SUM(CASE WHEN products.active = true AND c.avg_unit_cost IS NOT NULL THEN products.stock * (products.price - c.avg_unit_cost) ELSE 0 END), 0) AS potential_profit,
		COALESCE(SUM(CASE WHEN products.active = true AND c.avg_unit_cost IS NOT NULL THEN products.stock * products.price ELSE 0 END), 0) AS sale_value_with_cost,
		COALESCE(SUM(CASE WHEN products.active = true THEN 1 ELSE 0 END), 0) AS products_total,
		COALESCE(SUM(CASE WHEN products.active = true AND c.avg_unit_cost IS NOT NULL THEN 1 ELSE 0 END), 0) AS products_with_cost`).
		Scan(&row).Error; err != nil {
		return prodRepo.ProductAggregates{}, err
	}
	return prodRepo.ProductAggregates{
		TotalValue:        row.TotalValue,
		LowCount:          row.LowCount,
		OutCount:          row.OutCount,
		CostValue:         row.CostValue,
		PotentialProfit:   row.PotentialProfit,
		SaleValueWithCost: row.SaleValueWithCost,
		ProductsTotal:     row.ProductsTotal,
		ProductsWithCost:  row.ProductsWithCost,
	}, nil
}

// ListUnitCosts — costo unitario promedio (pesos) por producto del filtro,
// solo de los que tienen al menos una entrada con costo. Reusa el mismo
// filtro que List/ListAggregates vía INNER JOIN a la subconsulta de costo,
// de modo que el mapa cubre exactamente el subconjunto con costo capturado.
func (r *ProductPostgresRepository) ListUnitCosts(tx sharedDomain.Transaction, q prodRepo.ListQuery) (map[uuid.UUID]float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	base := buildProductFilterPg(gormTx.Model(&models.ProductModel{}), q)
	costSub := avgUnitCostSubqueryPg(gormTx, q.GymID)
	var rows []struct {
		ID          uuid.UUID
		AvgUnitCost float64
	}
	if err := base.Session(&gorm.Session{}).
		Joins("JOIN (?) AS c ON c.product_id = products.id", costSub).
		Select("products.id AS id, c.avg_unit_cost AS avg_unit_cost").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]float64, len(rows))
	for _, x := range rows {
		out[x.ID] = x.AvgUnitCost
	}
	return out, nil
}

// avgUnitCostSubqueryPg construye la subconsulta de costo unitario
// promedio ponderado por cantidad por producto (pesos, 2 decimales).
// `cost` en stock_movements es el costo UNITARIO de la entrada (igual que
// el KPI de egresos, que totaliza cost*delta); el promedio ponderado es
// SUM(cost*delta)/SUM(delta). Una sola definición reusada por
// ListAggregates y ListUnitCosts para que el redondeo sea idéntico.
func avgUnitCostSubqueryPg(gormTx *gorm.DB, gymID uuid.UUID) *gorm.DB {
	return gormTx.Model(&models.StockMovementModel{}).
		Select("product_id, ROUND(SUM(cost * delta) / SUM(delta), 2) AS avg_unit_cost").
		Where("gym_id = ? AND movement_type = ? AND cost IS NOT NULL AND deleted_at IS NULL",
			gymID, stockMovementDomain.TypeRestock).
		Group("product_id").
		Having("SUM(delta) > 0")
}

// buildProductFilterPg — extraído del cuerpo de List para reuso con
// ListAggregates. Mismas reglas que buildProductWhereSqlite — los dos
// implementations se mantienen en lockstep.
func buildProductFilterPg(base *gorm.DB, q prodRepo.ListQuery) *gorm.DB {
	base = base.Where("gym_id = ? AND deleted_at IS NULL", q.GymID)
	switch q.ActiveFilter {
	case prodRepo.ActiveFilterAll:
		// no filter
	case prodRepo.ActiveFilterInactive:
		base = base.Where("active = ?", false)
	default:
		base = base.Where("active = ?", true)
	}
	if q.Category != "" {
		base = base.Where("category = ?", q.Category)
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		// Substring (%s%) — antes era prefix; el FE buscaba "mineral"
		// y no encontraba "Agua mineral". Mismo cambio en SQLite.
		base = base.Where("LOWER(name) LIKE ?", "%"+strings.ToLower(s)+"%")
	}
	if q.LowStockOnly {
		base = base.Where("stock <= stock_minimum")
	}
	return base
}

// sortClausePostgres — whitelist explícita igual que la versión SQLite.
// Cualquier valor desconocido cae al default `LOWER(name) ASC`. Sin
// esto el query string del cliente podría inyectar columnas arbitrarias.
func sortClausePostgres(sort, dir string) string {
	col := "LOWER(name)"
	switch sort {
	case prodRepo.SortPrice:
		col = "price"
	case prodRepo.SortStock:
		col = "stock"
	case prodRepo.SortCategory:
		col = "LOWER(category)"
	}
	direction := "ASC"
	if dir == prodRepo.SortDirDesc {
		direction = "DESC"
	}
	if sort == prodRepo.SortName || sort == "" {
		return col + " " + direction
	}
	return col + " " + direction + ", LOWER(name) ASC"
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func productToModel(p *productDomain.Product) models.ProductModel {
	return models.ProductModel{
		ID:           p.ID,
		GymID:        p.GymID,
		Version:      p.Version,
		CreatedAt:    p.CreatedAt,
		UpdatedAt:    p.UpdatedAt,
		DeletedAt:    p.DeletedAt,
		Name:         p.Name,
		Price:        p.Price,
		Stock:        p.Stock,
		StockMinimum: p.StockMinimum,
		Category:     p.Category,
		ImageURL:     p.ImageURL,
		Active:       p.Active,
	}
}

func productFromModel(m *models.ProductModel) *productDomain.Product {
	return &productDomain.Product{
		ID:           m.ID,
		GymID:        m.GymID,
		Version:      m.Version,
		Name:         m.Name,
		Price:        m.Price,
		Stock:        m.Stock,
		StockMinimum: m.StockMinimum,
		Category:     m.Category,
		ImageURL:     m.ImageURL,
		Active:       m.Active,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
		DeletedAt:    m.DeletedAt,
	}
}
