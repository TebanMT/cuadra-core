//go:build server

package sync

import (
	"context"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// Include tombstones: an old SQLite also rejects a deleted zero-amount row.
func (s *PostgresStore) RequiresProductCreditSchema(ctx context.Context, tx shared.Transaction, gymID uuid.UUID) (bool, error) {
	var required bool
	err := gormTx(tx).WithContext(ctx).Raw(`SELECT EXISTS(SELECT 1 FROM payments WHERE gym_id=? AND concept='product' AND amount=0)`, gymID).Scan(&required).Error
	return required, err
}
