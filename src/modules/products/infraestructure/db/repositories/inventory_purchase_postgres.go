//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchaseDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type InventoryPurchasePostgresRepository struct{}

func NewInventoryPurchasePostgresRepository() *InventoryPurchasePostgresRepository {
	return &InventoryPurchasePostgresRepository{}
}

func (r *InventoryPurchasePostgresRepository) Create(tx sharedDomain.Transaction, p *purchaseDomain.Purchase) (*purchaseDomain.Purchase, error) {
	row := purchaseToModel(p)
	if err := tx.(*sharedDomain.GormTransaction).Tx.Create(&row).Error; err != nil {
		return nil, err
	}
	if err := emitInventoryPurchase(tx.(*sharedDomain.GormTransaction).Tx, p); err != nil {
		return nil, err
	}
	return purchaseFromModel(&row), nil
}

func emitInventoryPurchase(g *gorm.DB, p *purchaseDomain.Purchase) error {
	payload, err := json.Marshal(map[string]any{
		"origin": p.Origin, "id": p.ID, "gym_id": p.GymID, "version": p.Version,
		"created_at": p.CreatedAt.UnixMilli(), "updated_at": p.UpdatedAt.UnixMilli(), "deleted_at": p.DeletedAt,
		"stock_movement_id": nullablePurchaseMovement(p.StockMovementID), "product_id": p.ProductID, "quantity": p.Quantity,
		"unit_cost": p.UnitCost, "total_amount": p.TotalAmount, "status": p.Status,
		"paid_on": formatPurchaseDay(p.PaidOn), "payment_method": p.PaymentMethod, "paid_from": p.PaidFrom,
		"cash_movement_id": p.CashMovementID, "idempotency_key": p.IdempotencyKey, "created_by": p.CreatedBy,
	})
	if err != nil {
		return err
	}
	if err := g.Exec(`INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at,deleted_at)
		VALUES(?,'inventory_purchases',?,?,?::jsonb,NOW(),?) ON CONFLICT(gym_id,entity_type,entity_id)
		DO UPDATE SET version=EXCLUDED.version,payload=EXCLUDED.payload,server_updated_at=NOW(),deleted_at=EXCLUDED.deleted_at`,
		p.GymID, p.ID, p.Version, string(payload), p.DeletedAt).Error; err != nil {
		return err
	}
	return nil
}

func (r *InventoryPurchasePostgresRepository) Update(tx sharedDomain.Transaction, p *purchaseDomain.Purchase, expectedVersion int) (*purchaseDomain.Purchase, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	res := g.Model(&models.InventoryPurchaseModel{}).Where("gym_id=? AND id=? AND version=? AND deleted_at IS NULL", p.GymID, p.ID, expectedVersion).
		Updates(map[string]any{"version": p.Version, "updated_at": p.UpdatedAt, "quantity": p.Quantity, "unit_cost": p.UnitCost,
			"total_amount": p.TotalAmount, "status": p.Status, "paid_on": p.PaidOn,
			"payment_method": p.PaymentMethod, "paid_from": p.PaidFrom, "cash_movement_id": p.CashMovementID})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, prodErrors.ErrPurchaseVersionConflict
	}
	if err := emitInventoryPurchase(g, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *InventoryPurchasePostgresRepository) GetByID(tx sharedDomain.Transaction, gymID, id uuid.UUID) (*purchaseDomain.Purchase, error) {
	var row models.InventoryPurchaseModel
	err := tx.(*sharedDomain.GormTransaction).Tx.Where("gym_id=? AND id=? AND deleted_at IS NULL", gymID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, prodErrors.ErrPurchaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return purchaseFromModel(&row), nil
}

func formatPurchaseDay(v *time.Time) any {
	if v == nil {
		return nil
	}
	return v.Format("2006-01-02")
}

func (r *InventoryPurchasePostgresRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*purchaseDomain.Purchase, error) {
	var row models.InventoryPurchaseModel
	err := tx.(*sharedDomain.GormTransaction).Tx.
		Where("gym_id=? AND idempotency_key=? AND deleted_at IS NULL", gymID, key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return purchaseFromModel(&row), nil
}

func (r *InventoryPurchasePostgresRepository) GetByStockMovement(tx sharedDomain.Transaction, gymID, stockMovementID uuid.UUID) (*purchaseDomain.Purchase, error) {
	var row models.InventoryPurchaseModel
	err := tx.(*sharedDomain.GormTransaction).Tx.
		Where("gym_id=? AND stock_movement_id=? AND deleted_at IS NULL", gymID, stockMovementID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, prodErrors.ErrPurchaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return purchaseFromModel(&row), nil
}

func (r *InventoryPurchasePostgresRepository) List(tx sharedDomain.Transaction, query prodRepo.InventoryPurchaseListQuery) ([]*purchaseDomain.Purchase, int, error) {
	page, pageSize := normalizePage(query.Page, query.PageSize)
	q := tx.(*sharedDomain.GormTransaction).Tx.Model(&models.InventoryPurchaseModel{}).
		Where("gym_id=? AND deleted_at IS NULL", query.GymID)
	if query.Status != "" && query.Status != "all" {
		q = q.Where("status=?", query.Status)
	}
	if query.ReceiptStatus == "pending" {
		q = q.Where(`stock_movement_id IS NULL AND status<>'annulled' AND NOT EXISTS(SELECT 1 FROM inventory_purchase_receipts r WHERE r.purchase_id=inventory_purchases.id AND r.gym_id=inventory_purchases.gym_id)`)
	}
	if query.ReceiptStatus == "received" {
		q = q.Where(`stock_movement_id IS NOT NULL OR EXISTS(SELECT 1 FROM inventory_purchase_receipts r WHERE r.purchase_id=inventory_purchases.id AND r.gym_id=inventory_purchases.gym_id)`)
	}
	if query.From != nil {
		q = q.Where("created_at>=?", query.From.UTC())
	}
	if query.To != nil {
		q = q.Where("created_at<?", query.To.UTC())
	}
	var total int64
	if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []models.InventoryPurchaseModel
	if err := q.Order("CASE status WHEN 'unpaid' THEN 0 ELSE 1 END,created_at DESC,id").
		Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*purchaseDomain.Purchase, len(rows))
	for i := range rows {
		out[i] = purchaseFromModel(&rows[i])
	}
	return out, int(total), nil
}

func purchaseToModel(p *purchaseDomain.Purchase) models.InventoryPurchaseModel {
	return models.InventoryPurchaseModel{
		Origin: p.Origin,
		ID:     p.ID, GymID: p.GymID, Version: p.Version, CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt, DeletedAt: p.DeletedAt,
		StockMovementID: nullablePurchaseMovement(p.StockMovementID), ProductID: p.ProductID,
		Quantity: p.Quantity, UnitCost: p.UnitCost, TotalAmount: p.TotalAmount,
		Status: p.Status, PaidOn: p.PaidOn, PaymentMethod: p.PaymentMethod,
		PaidFrom: p.PaidFrom, CashMovementID: p.CashMovementID,
		IdempotencyKey: p.IdempotencyKey, CreatedBy: p.CreatedBy,
	}
}

func purchaseFromModel(m *models.InventoryPurchaseModel) *purchaseDomain.Purchase {
	return &purchaseDomain.Purchase{
		Origin: m.Origin,
		ID:     m.ID, GymID: m.GymID, Version: m.Version, CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt, DeletedAt: m.DeletedAt,
		StockMovementID: purchaseMovementValue(m.StockMovementID), ProductID: m.ProductID,
		Quantity: m.Quantity, UnitCost: m.UnitCost, TotalAmount: m.TotalAmount,
		Status: m.Status, PaidOn: m.PaidOn, PaymentMethod: m.PaymentMethod,
		PaidFrom: m.PaidFrom, CashMovementID: m.CashMovementID,
		IdempotencyKey: m.IdempotencyKey, CreatedBy: m.CreatedBy,
	}
}

func nullablePurchaseMovement(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
func purchaseMovementValue(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func (r *InventoryPurchasePostgresRepository) LockRegistration(tx sharedDomain.Transaction, gymID, id uuid.UUID) error {
	return tx.(*sharedDomain.GormTransaction).Tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, gymID.String()+":purchase-registration:"+id.String()).Error
}
