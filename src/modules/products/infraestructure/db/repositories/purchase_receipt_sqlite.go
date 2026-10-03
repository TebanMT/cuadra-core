//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	sqlite3 "github.com/mattn/go-sqlite3"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchase "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

type InventoryPurchaseReceiptSQLiteRepository struct{}

func NewInventoryPurchaseReceiptSQLiteRepository() *InventoryPurchaseReceiptSQLiteRepository {
	return &InventoryPurchaseReceiptSQLiteRepository{}
}
func (r *InventoryPurchaseReceiptSQLiteRepository) GetByPurchase(tx shared.Transaction, gymID, id uuid.UUID) (*purchase.Receipt, error) {
	var row struct {
		ID         string `db:"id"`
		GymID      string `db:"gym_id"`
		PurchaseID string `db:"purchase_id"`
		ProductID  string `db:"product_id"`
		ReceivedBy string `db:"received_by"`
		Quantity   int    `db:"quantity"`
		UnitCost   int64  `db:"unit_cost"`
		CreatedAt  int64  `db:"created_at"`
	}
	err := tx.(*shared.SqlxTransaction).Get(context.Background(), &row, `SELECT id,gym_id,purchase_id,product_id,received_by,quantity,unit_cost,created_at FROM inventory_purchase_receipts WHERE gym_id=? AND purchase_id=?`, gymID.String(), id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &purchase.Receipt{ID: uuid.MustParse(row.ID), GymID: uuid.MustParse(row.GymID), PurchaseID: uuid.MustParse(row.PurchaseID), ProductID: uuid.MustParse(row.ProductID), ReceivedBy: uuid.MustParse(row.ReceivedBy), Quantity: row.Quantity, UnitCost: fromCents(row.UnitCost), CreatedAt: time.UnixMilli(row.CreatedAt).UTC()}, nil
}
func (r *InventoryPurchaseReceiptSQLiteRepository) Create(tx shared.Transaction, receipt *purchase.Receipt) (*purchase.Receipt, error) {
	return r.Apply(tx, receipt, true)
}

// Apply runs inside the caller's transaction. Retried/full-sync events never
// touch the payment or increment stock twice. Derived stock rows stay off queue.
func (r *InventoryPurchaseReceiptSQLiteRepository) Apply(tx shared.Transaction, v *purchase.Receipt, enqueue bool) (*purchase.Receipt, error) {
	stx := tx.(*shared.SqlxTransaction)
	ctx := context.Background()
	existing, err := r.GetByPurchase(tx, v.GymID, v.PurchaseID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !existing.SameDelivery(v) {
			return nil, prodErrors.ErrPurchaseReceiptConflict
		}
		if !enqueue {
			// Canonical metadata may come from another terminal that confirmed
			// the same delivery first. The accepted physical quantity remains immutable.
			if _, err = stx.Exec(ctx, `UPDATE inventory_purchase_receipts SET unit_cost=?,received_by=?,created_at=?,updated_at=?,synced_at=? WHERE id=? AND gym_id=?`, toCents(v.UnitCost), v.ReceivedBy.String(), v.CreatedAt.UnixMilli(), v.CreatedAt.UnixMilli(), time.Now().UTC().UnixMilli(), v.ID.String(), v.GymID.String()); err != nil {
				return nil, err
			}
			if _, err = stx.Exec(ctx, `UPDATE stock_movements SET cost=?,operator_id=?,created_at=?,updated_at=? WHERE id=? AND gym_id=?`, toCents(v.UnitCost), v.ReceivedBy.String(), v.CreatedAt.UnixMilli(), v.CreatedAt.UnixMilli(), v.StockMovementID().String(), v.GymID.String()); err != nil {
				return nil, err
			}
			return v, nil
		}
		return existing, nil
	}
	var valid int
	err = stx.Get(ctx, &valid, `SELECT COUNT(*) FROM inventory_purchases p JOIN products pr ON pr.id=p.product_id AND pr.gym_id=p.gym_id JOIN users u ON u.id=? AND u.gym_id=p.gym_id WHERE p.id=? AND p.gym_id=? AND p.product_id=? AND p.stock_movement_id IS NULL`, v.ReceivedBy.String(), v.PurchaseID.String(), v.GymID.String(), v.ProductID.String())
	if err != nil {
		return nil, err
	}
	if valid != 1 {
		if !enqueue {
			var parents int
			if err := stx.Get(ctx, &parents, `SELECT (SELECT COUNT(*) FROM inventory_purchases WHERE id=?)+(SELECT COUNT(*) FROM products WHERE id=?)+(SELECT COUNT(*) FROM users WHERE id=?)`, v.PurchaseID.String(), v.ProductID.String(), v.ReceivedBy.String()); err != nil {
				return nil, err
			}
			if parents < 3 {
				return nil, sqlite3.Error{Code: sqlite3.ErrConstraint, ExtendedCode: sqlite3.ErrConstraintForeignKey}
			}
		}
		return nil, prodErrors.ErrPurchaseReceiptInvalid
	}
	_, err = stx.Exec(ctx, `INSERT INTO inventory_purchase_receipts(id,gym_id,version,created_at,updated_at,purchase_id,product_id,quantity,unit_cost,received_by) VALUES(?,?,1,?,?,?,?,?,?,?)`, v.ID.String(), v.GymID.String(), v.CreatedAt.UnixMilli(), v.CreatedAt.UnixMilli(), v.PurchaseID.String(), v.ProductID.String(), v.Quantity, toCents(v.UnitCost), v.ReceivedBy.String())
	if err != nil {
		return nil, err
	}
	_, err = stx.Exec(ctx, `INSERT INTO stock_movements(id,gym_id,version,created_at,updated_at,product_id,movement_type,delta,reason,cost,is_purchase,operator_id,idempotency_key,idempotency_fingerprint,idempotency_result) VALUES(?,?,1,?,?,?,'restock',?,'Recepción de compra',?,0,?,?,'purchase-receipt','{}')`, v.StockMovementID().String(), v.GymID.String(), v.CreatedAt.UnixMilli(), v.CreatedAt.UnixMilli(), v.ProductID.String(), v.Quantity, toCents(v.UnitCost), v.ReceivedBy.String(), "purchase-receipt:"+v.ID.String())
	if err != nil {
		return nil, err
	}
	_, err = stx.Exec(ctx, `UPDATE products SET stock_base=COALESCE(stock_base,stock),stock=COALESCE(stock_base,stock)+(SELECT COALESCE(SUM(quantity),0) FROM inventory_purchase_receipts r WHERE r.product_id=products.id AND r.gym_id=products.gym_id) WHERE id=? AND gym_id=?`, v.ProductID.String(), v.GymID.String())
	if err != nil {
		return nil, err
	}
	if enqueue {
		payload, e := ReceiptPayload(v)
		if e != nil {
			return nil, e
		}
		if e = stx.EnqueueSync(ctx, "inventory_purchase_receipts", v.ID.String(), "upsert", payload, 1); e != nil {
			return nil, e
		}
	}
	return v, nil
}
func ReceiptStockSQLite(tx *shared.SqlxTransaction, gymID, productID string) (int, error) {
	var total int
	err := tx.Get(context.Background(), &total, `SELECT COALESCE(SUM(quantity),0) FROM inventory_purchase_receipts WHERE gym_id=? AND product_id=?`, gymID, productID)
	return total, err
}
