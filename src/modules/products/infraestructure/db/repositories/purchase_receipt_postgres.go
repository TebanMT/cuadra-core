//go:build server

package repositories

import (
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchase "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	"github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

type InventoryPurchaseReceiptPostgresRepository struct{}

func NewInventoryPurchaseReceiptPostgresRepository() *InventoryPurchaseReceiptPostgresRepository {
	return &InventoryPurchaseReceiptPostgresRepository{}
}
func (r *InventoryPurchaseReceiptPostgresRepository) GetByPurchase(tx shared.Transaction, gymID, id uuid.UUID) (*purchase.Receipt, error) {
	var v models.PurchaseReceiptModel
	err := tx.(*shared.GormTransaction).Tx.Table("inventory_purchase_receipts").Where("gym_id=? AND purchase_id=?", gymID, id).Take(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &purchase.Receipt{ID: v.ID, GymID: v.GymID, PurchaseID: v.PurchaseID, ProductID: v.ProductID, ReceivedBy: v.ReceivedBy, Quantity: v.Quantity, UnitCost: v.UnitCost, CreatedAt: v.CreatedAt}, nil
}
func (r *InventoryPurchaseReceiptPostgresRepository) Create(tx shared.Transaction, v *purchase.Receipt) (*purchase.Receipt, error) {
	g := tx.(*shared.GormTransaction).Tx
	// Serialize on the purchase even before the first receipt exists.
	if err := g.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, v.GymID.String()+":receipt:"+v.PurchaseID.String()).Error; err != nil {
		return nil, err
	}
	existing, err := r.GetByPurchase(tx, v.GymID, v.PurchaseID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !existing.SameDelivery(v) {
			return nil, prodErrors.ErrPurchaseReceiptConflict
		}
		return existing, nil
	}
	var valid int64
	err = g.Raw(`SELECT COUNT(*) FROM inventory_purchases p JOIN products pr ON pr.id=p.product_id AND pr.gym_id=p.gym_id JOIN users u ON u.id=? AND u.gym_id=p.gym_id WHERE p.id=? AND p.gym_id=? AND p.product_id=? AND p.stock_movement_id IS NULL`, v.ReceivedBy, v.PurchaseID, v.GymID, v.ProductID).Scan(&valid).Error
	if err != nil {
		return nil, err
	}
	if valid != 1 {
		return nil, prodErrors.ErrPurchaseReceiptInvalid
	}
	// Lock product before adding an event; product snapshot projection takes the
	// same lock before reading the receipt total.
	if err = g.Exec(`SELECT id FROM products WHERE id=? AND gym_id=? FOR UPDATE`, v.ProductID, v.GymID).Error; err != nil {
		return nil, err
	}
	err = g.Exec(`INSERT INTO inventory_purchase_receipts(id,gym_id,version,created_at,updated_at,purchase_id,product_id,quantity,unit_cost,received_by) VALUES(?,?,1,?,?,?,?,?,?,?)`, v.ID, v.GymID, v.CreatedAt, v.CreatedAt, v.PurchaseID, v.ProductID, v.Quantity, v.UnitCost, v.ReceivedBy).Error
	if err != nil {
		return nil, err
	}
	err = g.Exec(`INSERT INTO stock_movements(id,gym_id,version,created_at,updated_at,product_id,movement_type,delta,reason,cost,is_purchase,operator_id,idempotency_key,idempotency_fingerprint,idempotency_result) VALUES(?,?,1,?,?,?,'restock',?,'Recepción de compra',?,FALSE,?,?,'purchase-receipt','{}')`, v.StockMovementID(), v.GymID, v.CreatedAt, v.CreatedAt, v.ProductID, v.Quantity, v.UnitCost, v.ReceivedBy, "purchase-receipt:"+v.ID.String()).Error
	if err != nil {
		return nil, err
	}
	if err = g.Exec(`UPDATE products SET stock_base=COALESCE(stock_base,stock),stock=COALESCE(stock_base,stock)+(SELECT COALESCE(SUM(quantity),0) FROM inventory_purchase_receipts r WHERE r.product_id=products.id AND r.gym_id=products.gym_id) WHERE id=? AND gym_id=?`, v.ProductID, v.GymID).Error; err != nil {
		return nil, err
	}
	payload, err := ReceiptPayload(v)
	if err != nil {
		return nil, err
	}
	err = g.Exec(`INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at) VALUES(?,'inventory_purchase_receipts',?,1,?::jsonb,clock_timestamp())`, v.GymID, v.ID, string(payload)).Error
	return v, err
}
func ReceiptStockPostgres(g *gorm.DB, gymID, productID uuid.UUID) (int, error) {
	var total int
	err := g.Raw(`SELECT COALESCE(SUM(quantity),0) FROM inventory_purchase_receipts WHERE gym_id=? AND product_id=?`, gymID, productID).Scan(&total).Error
	return total, err
}
