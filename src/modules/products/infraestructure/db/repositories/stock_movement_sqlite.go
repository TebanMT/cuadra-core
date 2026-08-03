//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type StockMovementSQLiteRepository struct{}

func NewStockMovementSQLiteRepository() *StockMovementSQLiteRepository {
	return &StockMovementSQLiteRepository{}
}

type sqliteStockMovementRow struct {
	ID           string         `db:"id"`
	GymID        string         `db:"gym_id"`
	Version      int            `db:"version"`
	CreatedAt    int64          `db:"created_at"`
	UpdatedAt    int64          `db:"updated_at"`
	DeletedAt    sql.NullInt64  `db:"deleted_at"`
	SyncedAt     sql.NullInt64  `db:"synced_at"`
	ProductID    string         `db:"product_id"`
	MovementType string         `db:"movement_type"`
	Delta        int            `db:"delta"`
	Reason       sql.NullString `db:"reason"`
	Cost         sql.NullInt64  `db:"cost"`
	IsPurchase   int            `db:"is_purchase"`
	SaleItemID   sql.NullString `db:"sale_item_id"`
	OperatorID   string         `db:"operator_id"`
}

func (r *StockMovementSQLiteRepository) Create(tx sharedDomain.Transaction, m *stockMovementDomain.StockMovement) (*stockMovementDomain.StockMovement, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := stockMovementToRow(m)
	const stmt = `
		INSERT INTO stock_movements (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    product_id, movement_type, delta, reason, cost, is_purchase, sale_item_id, operator_id
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :product_id, :movement_type, :delta, :reason, :cost, :is_purchase, :sale_item_id, :operator_id
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueStockMovement(stx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *StockMovementSQLiteRepository) ListByProduct(tx sharedDomain.Transaction, productID uuid.UUID, limit int) ([]*stockMovementDomain.StockMovement, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []sqliteStockMovementRow
	if err := stx.Select(context.Background(), &rows,
		`SELECT * FROM stock_movements WHERE product_id = ? AND deleted_at IS NULL
		 ORDER BY created_at DESC LIMIT ?`, productID.String(), limit); err != nil {
		return nil, err
	}
	out := make([]*stockMovementDomain.StockMovement, len(rows))
	for i := range rows {
		out[i] = stockMovementFromRow(&rows[i])
	}
	return out, nil
}

func stockMovementToRow(m *stockMovementDomain.StockMovement) sqliteStockMovementRow {
	row := sqliteStockMovementRow{
		ID:           m.ID.String(),
		GymID:        m.GymID.String(),
		Version:      m.Version,
		CreatedAt:    m.CreatedAt.UnixMilli(),
		UpdatedAt:    m.UpdatedAt.UnixMilli(),
		ProductID:    m.ProductID.String(),
		MovementType: m.MovementType,
		Delta:        m.Delta,
		OperatorID:   m.OperatorID.String(),
	}
	if m.IsPurchase {
		row.IsPurchase = 1
	}
	if m.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: m.DeletedAt.UnixMilli(), Valid: true}
	}
	if m.Reason != nil {
		row.Reason = sql.NullString{String: *m.Reason, Valid: true}
	}
	if m.Cost != nil {
		row.Cost = sql.NullInt64{Int64: toCents(*m.Cost), Valid: true}
	}
	if m.SaleItemID != nil {
		row.SaleItemID = sql.NullString{String: m.SaleItemID.String(), Valid: true}
	}
	return row
}

func stockMovementFromRow(r *sqliteStockMovementRow) *stockMovementDomain.StockMovement {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	prodID, _ := uuid.Parse(r.ProductID)
	opID, _ := uuid.Parse(r.OperatorID)
	m := &stockMovementDomain.StockMovement{
		ID:           id,
		GymID:        gymID,
		Version:      r.Version,
		ProductID:    prodID,
		MovementType: r.MovementType,
		Delta:        r.Delta,
		IsPurchase:   r.IsPurchase != 0,
		OperatorID:   opID,
		CreatedAt:    time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:    time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		m.DeletedAt = &t
	}
	if r.Reason.Valid {
		s := r.Reason.String
		m.Reason = &s
	}
	if r.Cost.Valid {
		c := fromCents(r.Cost.Int64)
		m.Cost = &c
	}
	if r.SaleItemID.Valid {
		sid, _ := uuid.Parse(r.SaleItemID.String)
		m.SaleItemID = &sid
	}
	return m
}

func enqueueStockMovement(stx *sharedDomain.SqlxTransaction, m *stockMovementDomain.StockMovement) error {
	if stx.Queue == nil {
		return nil
	}
	// All NOT NULL columns must be in the payload — the cloud projector's
	// UPSERT only emits columns present in the map, and a missing required
	// column on first-sight INSERT triggers a 23502 NOT NULL violation.
	var cost any
	if m.Cost != nil {
		cost = *m.Cost
	}
	payload, err := json.Marshal(map[string]any{
		"id":            m.ID.String(),
		"gym_id":        m.GymID.String(),
		"version":       m.Version,
		"created_at":    m.CreatedAt.UnixMilli(),
		"updated_at":    m.UpdatedAt.UnixMilli(),
		"product_id":    m.ProductID.String(),
		"movement_type": m.MovementType,
		"delta":         m.Delta,
		"reason":        strPtrOrNil(m.Reason),
		"cost":          cost,
		"is_purchase":   m.IsPurchase,
		"sale_item_id":  uuidPtrOrNil(m.SaleItemID),
		"operator_id":   m.OperatorID.String(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "stock_movements", m.ID.String(), "upsert", payload, m.Version)
}
