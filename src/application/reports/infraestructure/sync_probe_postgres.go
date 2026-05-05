//go:build server

package infraestructure

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// SyncProbe returns a closure suitable for ReportsController.WithSyncProbe.
// Runs a single MAX(updated_at) query across the projector-fed tables for
// the gym (members, payments, checkins). When all are empty/null, returns
// (nil, nil) so the FE renders "sin actividad reciente" cleanly.
func SyncProbe(uow sharedDomain.UnitOfWork) func(ctx context.Context, gymID uuid.UUID) (*time.Time, error) {
	return func(ctx context.Context, gymID uuid.UUID) (*time.Time, error) {
		tx, err := uow.Query(ctx)
		if err != nil {
			return nil, err
		}
		gormTx, ok := tx.(*sharedDomain.GormTransaction)
		if !ok || gormTx == nil {
			return nil, nil
		}
		var row struct {
			Newest *time.Time
		}
		err = gormTx.Tx.Raw(`
			SELECT GREATEST(
			    COALESCE((SELECT MAX(updated_at) FROM members  WHERE gym_id = ? AND deleted_at IS NULL), TIMESTAMP 'epoch'),
			    COALESCE((SELECT MAX(updated_at) FROM payments WHERE gym_id = ? AND deleted_at IS NULL), TIMESTAMP 'epoch'),
			    COALESCE((SELECT MAX(updated_at) FROM checkins WHERE gym_id = ? AND deleted_at IS NULL), TIMESTAMP 'epoch')
			) AS newest
		`, gymID, gymID, gymID).Scan(&row).Error
		if err != nil {
			// `record not found` from MAX() — gorm raises gorm.ErrRecordNotFound
			// when the projection is empty. Translate to "no data".
			if err == gorm.ErrRecordNotFound {
				return nil, nil
			}
			return nil, err
		}
		if row.Newest == nil || row.Newest.IsZero() ||
			row.Newest.Year() <= 1970 { // epoch sentinel from COALESCE above.
			return nil, nil
		}
		t := row.Newest.UTC()
		return &t, nil
	}
}
