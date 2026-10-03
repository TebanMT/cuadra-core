//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"errors"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"time"
)

type legacyPurchaseCostSQLiteRow struct {
	MovementID  uuid.UUID     `db:"movement_id"`
	ProductID   uuid.UUID     `db:"product_id"`
	ProductName string        `db:"product_name"`
	Quantity    int           `db:"quantity"`
	Version     int           `db:"version"`
	OccurredAt  int64         `db:"occurred_at"`
	Cost        sql.NullInt64 `db:"cost"`
}

func (r legacyPurchaseCostSQLiteRow) domain() prodRepo.LegacyPurchaseCost {
	out := prodRepo.LegacyPurchaseCost{MovementID: r.MovementID, ProductID: r.ProductID, ProductName: r.ProductName, Quantity: r.Quantity, Version: r.Version, OccurredAt: time.UnixMilli(r.OccurredAt).UTC()}
	if r.Cost.Valid {
		cost := float64(r.Cost.Int64) / 100
		out.Cost = &cost
	}
	return out
}

func (r *InventoryPurchaseSQLiteRepository) ListMissingPurchaseCosts(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, page, pageSize int) ([]prodRepo.LegacyPurchaseCost, int, error) {
	page, pageSize = normalizePage(page, pageSize)
	query := ` FROM stock_movements sm JOIN products p ON p.id=sm.product_id AND p.gym_id=sm.gym_id LEFT JOIN inventory_purchases ip ON ip.stock_movement_id=sm.id AND ip.gym_id=sm.gym_id AND ip.deleted_at IS NULL WHERE sm.gym_id=? AND sm.deleted_at IS NULL AND sm.movement_type='restock' AND sm.is_purchase=1 AND (ip.id IS NULL OR ip.status='legacy_incomplete') AND COALESCE(NULLIF(ip.total_amount,0),CASE WHEN sm.delta>0 AND sm.cost>0 THEN sm.delta*sm.cost END,0)<=0 AND sm.created_at>=? AND sm.created_at<?`
	args := []any{gymID.String(), from.UnixMilli(), to.UnixMilli()}
	var count int64
	var rows []legacyPurchaseCostSQLiteRow
	s := tx.(*sharedDomain.SqlxTransaction)
	if err := s.Get(context.Background(), &count, "SELECT COUNT(*)"+query, args...); err != nil {
		return nil, 0, err
	}
	if err := s.Select(context.Background(), &rows, `SELECT sm.id AS movement_id,sm.product_id,p.name AS product_name,sm.delta AS quantity,sm.version,sm.created_at AS occurred_at,sm.cost`+query+` ORDER BY sm.created_at DESC,sm.id LIMIT ? OFFSET ?`, append(args, pageSize, (page-1)*pageSize)...); err != nil {
		return nil, 0, err
	}
	out := make([]prodRepo.LegacyPurchaseCost, len(rows))
	for i, row := range rows {
		out[i] = row.domain()
	}
	return out, int(count), nil
}
func (r *InventoryPurchaseSQLiteRepository) GetLegacyPurchaseMovement(tx sharedDomain.Transaction, gymID, movementID uuid.UUID) (*prodRepo.LegacyPurchaseCost, error) {
	var row legacyPurchaseCostSQLiteRow
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &row, `SELECT sm.id AS movement_id,sm.product_id,p.name AS product_name,sm.delta AS quantity,sm.version,sm.created_at AS occurred_at,sm.cost FROM stock_movements sm JOIN products p ON p.id=sm.product_id AND p.gym_id=sm.gym_id WHERE sm.gym_id=? AND sm.deleted_at IS NULL AND sm.movement_type='restock' AND sm.is_purchase=1 AND sm.id=?`, gymID.String(), movementID.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, prodErrors.ErrPurchaseNotFound
	}
	if err != nil {
		return nil, err
	}
	out := row.domain()
	return &out, nil
}
