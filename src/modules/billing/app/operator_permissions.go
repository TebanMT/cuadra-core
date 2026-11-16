package app

import (
	"context"
	"math"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Operator permissions that depend on what a charge contains (UC-007). The
// route-level ones live in middleware.EnforceOperatorPermissions. Every
// helper is a no-op when the actor may do it, including calls without grants
// in the context (jobs, tests, internal flows), and only then reads the gym.

func allowed(ctx context.Context, p sharedDomain.Permission) bool {
	return sharedDomain.RequirePermission(ctx, p) == nil
}

// requireNotBackdated: a charge dated before the gym's today needs
// PermDiscounts. resolveMonetaryDate already refused future dates.
func requireNotBackdated(ctx context.Context, tx sharedDomain.Transaction, gyms gymRepo.GymRepository, gymID uuid.UUID, day, now time.Time) error {
	if allowed(ctx, sharedDomain.PermDiscounts) {
		return nil
	}
	today, err := strictGymLocalPaymentDate(tx, gyms, gymID, now)
	if err != nil {
		return err
	}
	return memApp.RequireTodayCharge(ctx, monetaryDateOnly(day), today)
}

// requireFullPayment: charging less than the total (fiado) needs PermCredit.
func requireFullPayment(ctx context.Context, paid, total float64) error {
	if toCents(paid) < toCents(total) {
		return sharedDomain.RequirePermission(ctx, sharedDomain.PermCredit)
	}
	return nil
}

// requireMembershipFees checks the enrollment / maintenance decision of a
// membership payment against the reception defaults (memApp.RequireDefaultFees).
func requireMembershipFees(ctx context.Context, tx sharedDomain.Transaction, gyms gymRepo.GymRepository, gymID uuid.UUID,
	member *memberDomain.Member, mt *mtDomain.MembershipType, day time.Time,
	chargeEnrollment bool, enrollmentAmt float64, chargeMaintenance bool, maintenanceAmt float64,
) error {
	if allowed(ctx, sharedDomain.PermDiscounts) {
		return nil
	}
	var defaults memApp.FeeDefaults
	if gyms != nil {
		g, err := gyms.GetByID(tx, gymID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		defaults = memApp.OperatorFeeDefaults(g, member, mt, day)
	} else {
		defaults = memApp.OperatorFeeDefaults(nil, member, mt, day)
	}
	return memApp.RequireDefaultFees(ctx, defaults, chargeEnrollment, enrollmentAmt, chargeMaintenance, maintenanceAmt)
}

func toCents(v float64) int64 { return int64(math.Round(v * 100)) }
