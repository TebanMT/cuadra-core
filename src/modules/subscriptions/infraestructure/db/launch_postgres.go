//go:build server

package db

import (
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"time"
)

type LaunchPostgresRepository struct{}

func NewLaunchPostgresRepository() *LaunchPostgresRepository { return &LaunchPostgresRepository{} }

func (*LaunchPostgresRepository) Claim(tx sharedDomain.Transaction, gymID uuid.UUID, paidAt time.Time) (bool, error) {
	db := tx.(*sharedDomain.GormTransaction).Tx
	// One transaction-wide lock for allocation. Slot CHECK + UNIQUE(gym_id) are
	// the final DB constraints; entries are never deleted or reused.
	// The lock key is stable across releases; its 50 is not the cohort limit.
	if err := db.Exec(`SELECT pg_advisory_xact_lock(20261014,50)`).Error; err != nil {
		return false, err
	}
	var existing int64
	if err := db.Table("launch_benefits").Where("gym_id = ?", gymID).Count(&existing).Error; err != nil {
		return false, err
	}
	if existing > 0 {
		return true, db.Exec(`UPDATE gyms SET all_features_access=TRUE WHERE id=? AND deleted_at IS NULL`, gymID).Error
	}
	var slot int
	if err := db.Raw(`SELECT tinta_private.launch_next_slot()`).Scan(&slot).Error; err != nil {
		return false, err
	}
	if slot > subDomain.LaunchLimit {
		return false, nil
	}
	if err := db.Exec(`INSERT INTO launch_benefits(gym_id,slot,first_paid_at) VALUES(?,?,?)`, gymID, slot, paidAt.UTC()).Error; err != nil {
		return false, err
	}
	if err := db.Exec(`UPDATE gyms SET all_features_access=TRUE WHERE id=? AND deleted_at IS NULL`, gymID).Error; err != nil {
		return false, err
	}
	return true, nil
}

func (*LaunchPostgresRepository) Remaining(tx sharedDomain.Transaction) (int, error) {
	db := tx.(*sharedDomain.GormTransaction).Tx
	var used int
	if err := db.Raw(`SELECT tinta_private.launch_used()`).Scan(&used).Error; err != nil {
		return 0, err
	}
	return max(0, subDomain.LaunchLimit-used), nil
}

// Serialize the entire subscription transition, including deduplication and
// the stale-event check. A retry must observe the committed payment/grant.
func (*EventPostgresRepository) LockGym(tx sharedDomain.Transaction, gymID uuid.UUID) error {
	return tx.(*sharedDomain.GormTransaction).Tx.Exec(`SELECT id FROM gyms WHERE id=? FOR UPDATE`, gymID).Error
}
