package purchase

import (
	"time"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
)

// Receipt is a physical fact, independent of later payment corrections. A full
// delivery has the purchase's ID on every terminal, including while offline.
// Partial deliveries are not represented by this command.
type Receipt struct {
	ID, GymID, PurchaseID, ProductID, ReceivedBy uuid.UUID
	Quantity                                     int
	UnitCost                                     float64
	CreatedAt                                    time.Time
}

func NewReceipt(p *Purchase, actor uuid.UUID, quantity int, now time.Time) (*Receipt, error) {
	if !p.HasSeparateReceipt() || p.Status == StatusAnnulled || actor == uuid.Nil || quantity <= 0 || quantity != p.Quantity || p.UnitCost == nil {
		return nil, prodErrors.ErrPurchaseReceiptInvalid
	}
	return &Receipt{ID: p.ID, GymID: p.GymID, PurchaseID: p.ID, ProductID: p.ProductID, ReceivedBy: actor, Quantity: quantity, UnitCost: *p.UnitCost, CreatedAt: now.UTC()}, nil
}
func (r *Receipt) StockMovementID() uuid.UUID {
	return uuid.NewSHA1(r.ID, []byte("purchase-receipt-stock"))
}

// Cost is a valuation snapshot, not the physical delivery identity. Two
// terminals may see different costs after a cloud payment correction. The
// first accepted receipt supplies the canonical valuation and actor/time.
func (r *Receipt) SameDelivery(other *Receipt) bool {
	return r.ID == other.ID && r.GymID == other.GymID && r.PurchaseID == other.PurchaseID && r.ProductID == other.ProductID && r.Quantity == other.Quantity
}
