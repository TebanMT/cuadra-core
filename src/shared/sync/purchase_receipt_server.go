//go:build server

package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	prodRepos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

func projectProductWithReceipts(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return err
	}
	if err := g.Exec(`SELECT id FROM products WHERE id=? AND gym_id=? FOR UPDATE`, entityID, gymID).Error; err != nil {
		return err
	}
	if stock, exists := raw["stock"]; exists {
		n, ok := projectorIntegralInt64(stock)
		if !ok {
			return fmt.Errorf("invalid stock")
		}
		if v, exists := raw["stock_base"]; exists {
			n, ok = projectorIntegralInt64(v)
			if !ok {
				return fmt.Errorf("invalid stock base")
			}
		}
		total, err := prodRepos.ReceiptStockPostgres(g, gymID, entityID)
		if err != nil {
			return err
		}
		raw["stock_base"] = n
		raw["stock"] = n + int64(total)
	}
	normalized, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return projectGeneric(g, *FindTable("products"), gymID, entityID, normalized)
}
func (s *PostgresStore) upsertReceipt(ctx context.Context, tx shared.Transaction, gymID, id uuid.UUID, item PushItem) (UpsertResult, error) {
	receipt, err := prodRepos.ReceiptFromPayload(item.Payload)
	if err != nil || receipt == nil || receipt.ID != id || receipt.GymID != gymID || item.ClientVersion != 1 || item.Operation != "upsert" {
		return UpsertResult{}, newFinancialConflict("la recepción debe conservar sus datos originales")
	}
	canonical, err := prodRepos.NewInventoryPurchaseReceiptPostgresRepository().Create(tx, receipt)
	if err != nil {
		if errors.Is(err, prodErrors.ErrPurchaseReceiptConflict) || errors.Is(err, prodErrors.ErrPurchaseReceiptInvalid) {
			return UpsertResult{}, newFinancialConflict("no se pudo conciliar la recepción: %v", err)
		}
		return UpsertResult{}, err
	}
	payload, err := prodRepos.ReceiptPayload(canonical)
	if err != nil {
		return UpsertResult{}, err
	}
	var stamped struct{ ServerUpdatedAt time.Time }
	if err = gormTx(tx).Raw(`SELECT server_updated_at FROM sync_entities WHERE gym_id=? AND entity_type='inventory_purchase_receipts' AND entity_id=?`, gymID, id).Scan(&stamped).Error; err != nil {
		return UpsertResult{}, err
	}
	// Return the first accepted actor/time as canonical, without bumping version.
	status := StatusAccepted
	if canonical.UnitCost != receipt.UnitCost || canonical.ReceivedBy != receipt.ReceivedBy || !canonical.CreatedAt.Equal(receipt.CreatedAt) {
		status = StatusConflictServerWins
	}
	return UpsertResult{Status: status, ServerVersion: 1, ServerUpdatedAt: stamped.ServerUpdatedAt, ServerPayload: payload}, nil
}
func (s *PostgresStore) RequiresReceiptSchema(ctx context.Context, tx shared.Transaction, gymID uuid.UUID) (bool, error) {
	var required bool
	err := gormTx(tx).WithContext(ctx).Raw(`SELECT EXISTS(SELECT 1 FROM inventory_purchases WHERE gym_id=? AND stock_movement_id IS NULL)`, gymID).Scan(&required).Error
	return required, err
}

func (s *PostgresStore) RequiresPurchaseRegistrationSchema(ctx context.Context, tx shared.Transaction, gymID uuid.UUID) (bool, error) {
	var required bool
	err := gormTx(tx).WithContext(ctx).Raw(`SELECT EXISTS(SELECT 1 FROM products WHERE gym_id=? AND (stock<0 OR stock_base<0)) OR EXISTS(SELECT 1 FROM inventory_purchases WHERE gym_id=? AND stock_movement_id IS NULL AND origin='desktop')`, gymID, gymID).Scan(&required).Error
	return required, err
}
