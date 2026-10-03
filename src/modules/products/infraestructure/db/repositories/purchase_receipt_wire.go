package repositories

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	purchase "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
)

// ReceiptPayload is shared by both storage edges. Money is always pesos on wire.
func ReceiptPayload(r *purchase.Receipt) ([]byte, error) {
	return json.Marshal(map[string]any{
		"id": r.ID, "gym_id": r.GymID, "version": 1, "created_at": r.CreatedAt.UnixMilli(), "updated_at": r.CreatedAt.UnixMilli(), "deleted_at": nil,
		"purchase_id": r.PurchaseID, "product_id": r.ProductID, "quantity": r.Quantity, "unit_cost": r.UnitCost, "received_by": r.ReceivedBy,
	})
}
func ReceiptFromPayload(data []byte) (*purchase.Receipt, error) {
	var wire struct {
		ID         uuid.UUID `json:"id"`
		GymID      uuid.UUID `json:"gym_id"`
		PurchaseID uuid.UUID `json:"purchase_id"`
		ProductID  uuid.UUID `json:"product_id"`
		ReceivedBy uuid.UUID `json:"received_by"`
		Quantity   int       `json:"quantity"`
		UnitCost   float64   `json:"unit_cost"`
		CreatedAt  int64     `json:"created_at"`
		Version    int       `json:"version"`
		DeletedAt  any       `json:"deleted_at"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	if wire.ID == uuid.Nil || wire.GymID == uuid.Nil || wire.ID != wire.PurchaseID || wire.ProductID == uuid.Nil || wire.ReceivedBy == uuid.Nil || wire.Quantity <= 0 || wire.Quantity > 2147483647 || wire.Version != 1 || wire.DeletedAt != nil || wire.CreatedAt <= 0 || wire.UnitCost <= 0 || wire.UnitCost > 9999999999.99 || math.Abs(wire.UnitCost*100-math.Round(wire.UnitCost*100)) > 0.0001 {
		return nil, fmt.Errorf("recepción de compra inválida")
	}
	return &purchase.Receipt{ID: wire.ID, GymID: wire.GymID, PurchaseID: wire.PurchaseID, ProductID: wire.ProductID, ReceivedBy: wire.ReceivedBy, Quantity: wire.Quantity, UnitCost: wire.UnitCost, CreatedAt: time.UnixMilli(wire.CreatedAt).UTC()}, nil
}
