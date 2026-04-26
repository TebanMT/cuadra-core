//go:build server

package repositories

import (
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	saleDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/sale"
	"github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type SalePostgresRepository struct{}

func NewSalePostgresRepository() *SalePostgresRepository { return &SalePostgresRepository{} }

func (r *SalePostgresRepository) Create(tx sharedDomain.Transaction, s *saleDomain.Sale) (*saleDomain.Sale, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := saleToModel(s)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return s, nil
}

func (r *SalePostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*saleDomain.Sale, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.SaleModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrSaleNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return saleFromModel(&row), nil
}

func (r *SalePostgresRepository) GetByPaymentID(tx sharedDomain.Transaction, paymentID uuid.UUID) (*saleDomain.Sale, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.SaleModel
	err := gormTx.Where("payment_id = ? AND deleted_at IS NULL", paymentID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrSaleNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return saleFromModel(&row), nil
}

type SaleItemPostgresRepository struct{}

func NewSaleItemPostgresRepository() *SaleItemPostgresRepository {
	return &SaleItemPostgresRepository{}
}

func (r *SaleItemPostgresRepository) CreateMany(tx sharedDomain.Transaction, items []*saleDomain.SaleItem) error {
	if len(items) == 0 {
		return nil
	}
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	rows := make([]models.SaleItemModel, len(items))
	for i, it := range items {
		rows[i] = saleItemToModel(it)
	}
	return gormTx.Create(&rows).Error
}

func (r *SaleItemPostgresRepository) ListBySale(tx sharedDomain.Transaction, saleID uuid.UUID) ([]*saleDomain.SaleItem, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.SaleItemModel
	if err := gormTx.Where("sale_id = ? AND deleted_at IS NULL", saleID).
		Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*saleDomain.SaleItem, len(rows))
	for i := range rows {
		out[i] = saleItemFromModel(&rows[i])
	}
	return out, nil
}

func saleToModel(s *saleDomain.Sale) models.SaleModel {
	return models.SaleModel{
		ID:        s.ID,
		GymID:     s.GymID,
		Version:   s.Version,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
		DeletedAt: s.DeletedAt,
		PaymentID: s.PaymentID,
		MemberID:  s.MemberID,
		Subtotal:  s.Subtotal,
		Discount:  s.Discount,
		Total:     s.Total,
	}
}

func saleFromModel(m *models.SaleModel) *saleDomain.Sale {
	return &saleDomain.Sale{
		ID:        m.ID,
		GymID:     m.GymID,
		Version:   m.Version,
		PaymentID: m.PaymentID,
		MemberID:  m.MemberID,
		Subtotal:  m.Subtotal,
		Discount:  m.Discount,
		Total:     m.Total,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
		DeletedAt: m.DeletedAt,
	}
}

func saleItemToModel(it *saleDomain.SaleItem) models.SaleItemModel {
	return models.SaleItemModel{
		ID:                  it.ID,
		GymID:               it.GymID,
		Version:             it.Version,
		CreatedAt:           it.CreatedAt,
		UpdatedAt:           it.UpdatedAt,
		DeletedAt:           it.DeletedAt,
		SaleID:              it.SaleID,
		ProductID:           it.ProductID,
		ProductNameSnapshot: it.ProductNameSnapshot,
		UnitPriceSnapshot:   it.UnitPriceSnapshot,
		Quantity:            it.Quantity,
		LineTotal:           it.LineTotal,
	}
}

func saleItemFromModel(m *models.SaleItemModel) *saleDomain.SaleItem {
	return &saleDomain.SaleItem{
		ID:                  m.ID,
		GymID:               m.GymID,
		Version:             m.Version,
		SaleID:              m.SaleID,
		ProductID:           m.ProductID,
		ProductNameSnapshot: m.ProductNameSnapshot,
		UnitPriceSnapshot:   m.UnitPriceSnapshot,
		Quantity:            m.Quantity,
		LineTotal:           m.LineTotal,
		CreatedAt:           m.CreatedAt,
		UpdatedAt:           m.UpdatedAt,
		DeletedAt:           m.DeletedAt,
	}
}
