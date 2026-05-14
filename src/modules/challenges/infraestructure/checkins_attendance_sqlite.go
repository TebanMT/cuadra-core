//go:build sidecar

package infraestructure

import (
	"context"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CheckinsAttendanceAdapter is the sidecar (SQLite) twin of the postgres
// adapter. Identical contract — counts allowed check-ins for a member
// inside a unix-ms range against the local checkins table.
type CheckinsAttendanceAdapter struct{}

func NewCheckinsAttendanceAdapter() *CheckinsAttendanceAdapter {
	return &CheckinsAttendanceAdapter{}
}

func (a *CheckinsAttendanceAdapter) CountInRange(tx sharedDomain.Transaction, gymID, memberID uuid.UUID, fromMs, toMs int64) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM checkins
		 WHERE gym_id = ? AND member_id = ? AND deleted_at IS NULL
		   AND result LIKE 'allowed%'
		   AND checkin_at >= ? AND checkin_at < ?`,
		gymID.String(), memberID.String(), fromMs, toMs)
	return n, err
}
