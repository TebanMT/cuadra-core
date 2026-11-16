//go:build server

package db

import (
	"time"

	"github.com/google/uuid"

	subApp "github.com/cuadra/cuadra-core/src/modules/subscriptions/app"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// TrialExpiryPostgresReader implements subApp.ExpiredTrialReader. With the
// tenant UoW each query runs inside one gym's scope (TenantWorker), so it
// returns that gym or nothing.
type TrialExpiryPostgresReader struct{}

func NewTrialExpiryPostgresReader() *TrialExpiryPostgresReader {
	return &TrialExpiryPostgresReader{}
}

func (r *TrialExpiryPostgresReader) FindExpiredTrials(tx sharedDomain.Transaction, now time.Time) ([]subApp.ExpiredTrial, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []struct {
		ID          uuid.UUID
		TrialEndsAt time.Time
	}
	err := gormTx.Raw(`
		SELECT id, trial_ends_at
		  FROM gyms
		 WHERE deleted_at IS NULL
		   AND subscription_plan = 'trial'
		   AND subscription_status = 'active'
		   AND trial_ends_at IS NOT NULL
		   AND trial_ends_at <= ?
		 ORDER BY trial_ends_at, id`, now).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]subApp.ExpiredTrial, len(rows))
	for i, row := range rows {
		out[i] = subApp.ExpiredTrial{GymID: row.ID, TrialEndsAt: row.TrialEndsAt}
	}
	return out, nil
}
