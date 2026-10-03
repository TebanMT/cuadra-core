//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchaseDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type InventoryPurchaseSQLiteRepository struct{}

func NewInventoryPurchaseSQLiteRepository() *InventoryPurchaseSQLiteRepository {
	return &InventoryPurchaseSQLiteRepository{}
}

type sqliteInventoryPurchaseRow struct {
	Origin          string         `db:"origin"`
	ID              string         `db:"id"`
	GymID           string         `db:"gym_id"`
	Version         int            `db:"version"`
	CreatedAt       int64          `db:"created_at"`
	UpdatedAt       int64          `db:"updated_at"`
	DeletedAt       sql.NullInt64  `db:"deleted_at"`
	SyncedAt        sql.NullInt64  `db:"synced_at"`
	StockMovementID sql.NullString `db:"stock_movement_id"`
	ProductID       string         `db:"product_id"`
	Quantity        int            `db:"quantity"`
	UnitCost        sql.NullInt64  `db:"unit_cost"`
	TotalAmount     sql.NullInt64  `db:"total_amount"`
	Status          string         `db:"status"`
	PaidOn          sql.NullString `db:"paid_on"`
	PaymentMethod   sql.NullString `db:"payment_method"`
	PaidFrom        sql.NullString `db:"paid_from"`
	CashMovementID  sql.NullString `db:"cash_movement_id"`
	IdempotencyKey  string         `db:"idempotency_key"`
	CreatedBy       string         `db:"created_by"`
}

const inventoryPurchaseColumns = `origin,id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
stock_movement_id,product_id,quantity,unit_cost,total_amount,status,paid_on,payment_method,
paid_from,cash_movement_id,idempotency_key,created_by`

func (r *InventoryPurchaseSQLiteRepository) Create(tx sharedDomain.Transaction, p *purchaseDomain.Purchase) (*purchaseDomain.Purchase, error) {
	row := purchaseToSQLiteRow(p)
	stmt := `INSERT INTO inventory_purchases(` + inventoryPurchaseColumns + `) VALUES(
		:origin,:id,:gym_id,:version,:created_at,:updated_at,:deleted_at,:synced_at,
		:stock_movement_id,:product_id,:quantity,:unit_cost,:total_amount,:status,:paid_on,
		:payment_method,:paid_from,:cash_movement_id,:idempotency_key,:created_by)`
	if _, err := tx.(*sharedDomain.SqlxTransaction).NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueInventoryPurchase(tx.(*sharedDomain.SqlxTransaction), p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *InventoryPurchaseSQLiteRepository) Update(tx sharedDomain.Transaction, p *purchaseDomain.Purchase, expectedVersion int) (*purchaseDomain.Purchase, error) {
	if p.IsCloudManaged() {
		return nil, sharedDomain.NewBusinessError(prodErrors.ErrRemotePurchaseCloudOnly, "")
	}
	row := purchaseToSQLiteRow(p)
	res, err := tx.(*sharedDomain.SqlxTransaction).NamedExec(context.Background(), `UPDATE inventory_purchases SET
		version=:version,updated_at=:updated_at,quantity=:quantity,unit_cost=:unit_cost,total_amount=:total_amount,status=:status,
		paid_on=:paid_on,payment_method=:payment_method,paid_from=:paid_from,cash_movement_id=:cash_movement_id
		WHERE gym_id=:gym_id AND id=:id AND version=:expected_version AND deleted_at IS NULL`, map[string]any{
		"version": row.Version, "updated_at": row.UpdatedAt, "quantity": row.Quantity, "unit_cost": nullInt64Value(row.UnitCost),
		"total_amount": nullInt64Value(row.TotalAmount), "status": row.Status, "paid_on": nullStringValue(row.PaidOn),
		"payment_method": nullStringValue(row.PaymentMethod), "paid_from": nullStringValue(row.PaidFrom),
		"cash_movement_id": nullStringValue(row.CashMovementID), "gym_id": row.GymID, "id": row.ID,
		"expected_version": expectedVersion,
	})
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, prodErrors.ErrPurchaseVersionConflict
	}
	if err := enqueueInventoryPurchase(tx.(*sharedDomain.SqlxTransaction), p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *InventoryPurchaseSQLiteRepository) GetByID(tx sharedDomain.Transaction, gymID, id uuid.UUID) (*purchaseDomain.Purchase, error) {
	var row sqliteInventoryPurchaseRow
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &row,
		`SELECT `+inventoryPurchaseColumns+` FROM inventory_purchases WHERE gym_id=? AND id=? AND deleted_at IS NULL`, gymID.String(), id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, prodErrors.ErrPurchaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return purchaseFromSQLiteRow(&row), nil
}

func (r *InventoryPurchaseSQLiteRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*purchaseDomain.Purchase, error) {
	var row sqliteInventoryPurchaseRow
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &row,
		`SELECT `+inventoryPurchaseColumns+` FROM inventory_purchases WHERE gym_id=? AND idempotency_key=? AND deleted_at IS NULL`, gymID.String(), key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return purchaseFromSQLiteRow(&row), nil
}

func (r *InventoryPurchaseSQLiteRepository) GetByStockMovement(tx sharedDomain.Transaction, gymID, stockMovementID uuid.UUID) (*purchaseDomain.Purchase, error) {
	var row sqliteInventoryPurchaseRow
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &row,
		`SELECT `+inventoryPurchaseColumns+` FROM inventory_purchases WHERE gym_id=? AND stock_movement_id=? AND deleted_at IS NULL`, gymID.String(), stockMovementID.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, prodErrors.ErrPurchaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return purchaseFromSQLiteRow(&row), nil
}

func (r *InventoryPurchaseSQLiteRepository) List(tx sharedDomain.Transaction, q prodRepo.InventoryPurchaseListQuery) ([]*purchaseDomain.Purchase, int, error) {
	page, pageSize := normalizePage(q.Page, q.PageSize)
	query := `SELECT ` + inventoryPurchaseColumns + ` FROM inventory_purchases WHERE gym_id=? AND deleted_at IS NULL`
	args := []any{q.GymID.String()}
	if q.Status != "" && q.Status != "all" {
		query += ` AND status=?`
		args = append(args, q.Status)
	}
	if q.ReceiptStatus == "pending" {
		query += ` AND stock_movement_id IS NULL AND status<>'annulled' AND NOT EXISTS(SELECT 1 FROM inventory_purchase_receipts r WHERE r.purchase_id=inventory_purchases.id AND r.gym_id=inventory_purchases.gym_id)`
	}
	if q.ReceiptStatus == "received" {
		query += ` AND (stock_movement_id IS NOT NULL OR EXISTS(SELECT 1 FROM inventory_purchase_receipts r WHERE r.purchase_id=inventory_purchases.id AND r.gym_id=inventory_purchases.gym_id))`
	}
	if q.From != nil {
		query += ` AND created_at>=?`
		args = append(args, q.From.UTC().UnixMilli())
	}
	if q.To != nil {
		query += ` AND created_at<?`
		args = append(args, q.To.UTC().UnixMilli())
	}
	var total int
	countQuery := `SELECT COUNT(*) FROM (` + query + `) AS filtered_inventory_purchases`
	if err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &total, countQuery, args...); err != nil {
		return nil, 0, err
	}
	query += ` ORDER BY CASE status WHEN 'unpaid' THEN 0 ELSE 1 END,created_at DESC,id LIMIT ? OFFSET ?`
	args = append(args, pageSize, (page-1)*pageSize)
	var rows []sqliteInventoryPurchaseRow
	if err := tx.(*sharedDomain.SqlxTransaction).Select(context.Background(), &rows, query, args...); err != nil {
		return nil, 0, err
	}
	out := make([]*purchaseDomain.Purchase, len(rows))
	for i := range rows {
		out[i] = purchaseFromSQLiteRow(&rows[i])
	}
	return out, total, nil
}

func nullInt64Value(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func nullStringValue(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

func purchaseToSQLiteRow(p *purchaseDomain.Purchase) sqliteInventoryPurchaseRow {
	r := sqliteInventoryPurchaseRow{
		Origin: p.Origin,
		ID:     p.ID.String(), GymID: p.GymID.String(), Version: p.Version,
		CreatedAt: p.CreatedAt.UnixMilli(), UpdatedAt: p.UpdatedAt.UnixMilli(),
		StockMovementID: sql.NullString{String: p.StockMovementID.String(), Valid: p.StockMovementID != uuid.Nil}, ProductID: p.ProductID.String(),
		Quantity: p.Quantity, Status: p.Status, IdempotencyKey: p.IdempotencyKey,
		CreatedBy: p.CreatedBy.String(),
	}
	if p.DeletedAt != nil {
		r.DeletedAt = sql.NullInt64{Int64: p.DeletedAt.UnixMilli(), Valid: true}
	}
	if p.UnitCost != nil {
		r.UnitCost = sql.NullInt64{Int64: toCents(*p.UnitCost), Valid: true}
	}
	if p.TotalAmount != nil {
		r.TotalAmount = sql.NullInt64{Int64: toCents(*p.TotalAmount), Valid: true}
	}
	if p.PaidOn != nil {
		r.PaidOn = sql.NullString{String: p.PaidOn.Format("2006-01-02"), Valid: true}
	}
	if p.PaymentMethod != nil {
		r.PaymentMethod = sql.NullString{String: *p.PaymentMethod, Valid: true}
	}
	if p.PaidFrom != nil {
		r.PaidFrom = sql.NullString{String: *p.PaidFrom, Valid: true}
	}
	if p.CashMovementID != nil {
		r.CashMovementID = sql.NullString{String: p.CashMovementID.String(), Valid: true}
	}
	return r
}

func purchaseFromSQLiteRow(r *sqliteInventoryPurchaseRow) *purchaseDomain.Purchase {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	movementID, _ := uuid.Parse(r.StockMovementID.String)
	productID, _ := uuid.Parse(r.ProductID)
	createdBy, _ := uuid.Parse(r.CreatedBy)
	p := &purchaseDomain.Purchase{
		Origin: r.Origin,
		ID:     id, GymID: gymID, Version: r.Version, StockMovementID: movementID,
		ProductID: productID, Quantity: r.Quantity, Status: r.Status,
		IdempotencyKey: r.IdempotencyKey, CreatedBy: createdBy,
		CreatedAt: time.UnixMilli(r.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		v := time.UnixMilli(r.DeletedAt.Int64).UTC()
		p.DeletedAt = &v
	}
	if r.UnitCost.Valid {
		v := fromCents(r.UnitCost.Int64)
		p.UnitCost = &v
	}
	if r.TotalAmount.Valid {
		v := fromCents(r.TotalAmount.Int64)
		p.TotalAmount = &v
	}
	if r.PaidOn.Valid {
		v, _ := time.Parse("2006-01-02", r.PaidOn.String)
		p.PaidOn = &v
	}
	if r.PaymentMethod.Valid {
		v := r.PaymentMethod.String
		p.PaymentMethod = &v
	}
	if r.PaidFrom.Valid {
		v := r.PaidFrom.String
		p.PaidFrom = &v
	}
	if r.CashMovementID.Valid {
		v, _ := uuid.Parse(r.CashMovementID.String)
		p.CashMovementID = &v
	}
	return p
}

func enqueueInventoryPurchase(stx *sharedDomain.SqlxTransaction, p *purchaseDomain.Purchase) error {
	if stx.Queue == nil {
		return nil
	}
	var unitCost, totalAmount, paidOn, method, paidFrom, cashMovementID any
	if p.UnitCost != nil {
		unitCost = *p.UnitCost // wire money is pesos, never SQLite cents.
	}
	if p.TotalAmount != nil {
		totalAmount = *p.TotalAmount
	}
	if p.PaidOn != nil {
		paidOn = p.PaidOn.Format("2006-01-02")
	}
	if p.PaymentMethod != nil {
		method = *p.PaymentMethod
	}
	if p.PaidFrom != nil {
		paidFrom = *p.PaidFrom
	}
	if p.CashMovementID != nil {
		cashMovementID = p.CashMovementID.String()
	}
	payload, err := json.Marshal(map[string]any{
		"origin": p.Origin, "id": p.ID.String(), "gym_id": p.GymID.String(), "version": p.Version,
		"created_at": p.CreatedAt.UnixMilli(), "updated_at": p.UpdatedAt.UnixMilli(),
		"deleted_at": nil, "stock_movement_id": nullStringValue(sql.NullString{String: p.StockMovementID.String(), Valid: p.StockMovementID != uuid.Nil}),
		"product_id": p.ProductID.String(), "quantity": p.Quantity,
		"unit_cost": unitCost, "total_amount": totalAmount, "status": p.Status,
		"paid_on": paidOn, "payment_method": method, "paid_from": paidFrom,
		"cash_movement_id": cashMovementID, "idempotency_key": p.IdempotencyKey,
		"created_by": p.CreatedBy.String(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "inventory_purchases", p.ID.String(), "upsert", payload, p.Version)
}

func (r *InventoryPurchaseSQLiteRepository) LockRegistration(tx sharedDomain.Transaction, gymID, id uuid.UUID) error {
	return nil // SQLite UnitOfWork serializes write transactions.
}
