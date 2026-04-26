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
	base := gormTx.Model(&models.ProductModel{}).Where("gym_id = ? AND deleted_at IS NULL", q.GymID)
	if !q.IncludeInactive {
		base = base.Where("active = ?", true)
	}
	if q.Category != "" {
		base = base.Where("category = ?", q.Category)
	}
	if q.Search != "" {
		base = base.Where("LOWER(name) LIKE ?", strings.ToLower(strings.TrimSpace(q.Search))+"%")
	}
	if q.LowStockOnly {
		base = base.Where("stock <= stock_minimum")
	}
	var total int64
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []models.ProductModel
	if err := base.Order("LOWER(name) ASC").
		Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*productDomain.Product, len(rows))
	for i := range rows {
		out[i] = productFromModel(&rows[i])
	}
	return out, int(total), nil
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
