package repository

import (
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"time"
)

// LegacyPurchaseCost preserves the original physical receipt. Completing its
// financial cost must never create a second stock or cash movement.
type LegacyPurchaseCost struct {
	MovementID  uuid.UUID `json:"movement_id"`
	ProductID   uuid.UUID `json:"product_id"`
	ProductName string    `json:"product_name"`
	Quantity    int       `json:"quantity"`
	Version     int       `json:"version"`
	OccurredAt  time.Time `json:"occurred_at"`
	RecordedOn  string    `json:"recorded_on"`
	Cost        *float64  `json:"-"`
}

type LegacyPurchaseCostRepository interface {
	ListMissingPurchaseCosts(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, page, pageSize int) ([]LegacyPurchaseCost, int, error)
	// Locks the original receipt when the engine supports row locks.
	GetLegacyPurchaseMovement(tx sharedDomain.Transaction, gymID, movementID uuid.UUID) (*LegacyPurchaseCost, error)
}
