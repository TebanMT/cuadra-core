//go:build server && !sidecar

// Package infraestructure ties the challenges (retos) module to other
// modules' persistence without breaking DDD layering — concretely, the
// AttendanceCounter interface lets the ranking + DQ flow ask "how many
// check-ins did this member log inside the window" without importing the
// checkins module's repository directly. Wiring layer constructs the
// adapter and injects it via the AttendanceCounter port.
package infraestructure

import (
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CheckinsAttendanceAdapter satisfies challenges/domain/repository.AttendanceCounter
// by issuing a single COUNT query against the checkins table on whatever
// transaction the caller passes in. Lives in the challenges module (not the
// checkins module) so the dependency direction stays one-way: challenges
// reads checkins, never the reverse.
type CheckinsAttendanceAdapter struct{}

func NewCheckinsAttendanceAdapter() *CheckinsAttendanceAdapter {
	return &CheckinsAttendanceAdapter{}
}

// CountInRange counts allowed check-ins for (gym, member) inside
// [fromMs, toMs) in unix-ms. We only count `result LIKE 'allowed%'` so a
// member who showed up but got rejected for expired membership doesn't
// rack up DQ-protecting check-ins.
func (a *CheckinsAttendanceAdapter) CountInRange(tx sharedDomain.Transaction, gymID, memberID uuid.UUID, fromMs, toMs int64) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	from := time.UnixMilli(fromMs).UTC()
	to := time.UnixMilli(toMs).UTC()
	var n int64
	err := gormTx.Raw(
		`SELECT COUNT(1) FROM checkins
		 WHERE gym_id = ? AND member_id = ? AND deleted_at IS NULL
		   AND result LIKE 'allowed%'
		   AND checkin_at >= ? AND checkin_at < ?`,
		gymID, memberID, from, to).Scan(&n).Error
	return int(n), err
}
