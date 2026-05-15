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
// agrega total_value (solo activos), low_count y out_count usando
// SUM(CASE WHEN ...). Sobre catálogos de 50-200 SKUs es trivial.
func (r *ProductPostgresRepository) ListAggregates(tx sharedDomain.Transaction, q prodRepo.ListQuery) (prodRepo.ProductAggregates, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	base := buildProductFilterPg(gormTx.Model(&models.ProductModel{}), q)
	var row struct {
		TotalValue float64
		LowCount   int
		OutCount   int
	}
	if err := base.Session(&gorm.Session{}).Select(`
		COALESCE(SUM(CASE WHEN active = true THEN price * stock ELSE 0 END), 0) AS total_value,
		COALESCE(SUM(CASE WHEN active = true AND stock > 0 AND stock <= stock_minimum THEN 1 ELSE 0 END), 0) AS low_count,
		COALESCE(SUM(CASE WHEN active = true AND stock = 0 THEN 1 ELSE 0 END), 0) AS out_count`).
		Scan(&row).Error; err != nil {
		return prodRepo.ProductAggregates{}, err
	}
	return prodRepo.ProductAggregates{
		TotalValue: row.TotalValue,
		LowCount:   row.LowCount,
		OutCount:   row.OutCount,
	}, nil
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
