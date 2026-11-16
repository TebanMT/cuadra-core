package app

import (
	"context"
	"encoding/json"
	"math"
	"time"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// FeeDefaults is what reception charges by default for a membership payment,
// computed the way the desktop offers it (PaymentModal, MemberForm): the
// plan's fee when it has one, otherwise the gym default, and only when the
// gym charges that fee (gyms.charge_settings). A zero amount means the gym
// does not charge that fee.
type FeeDefaults struct {
	Enrollment     float64
	EnrollmentDue  bool
	Maintenance    float64
	MaintenanceDue bool
}

// OperatorFeeDefaults computes the reception defaults for member paying mt
// on day. g may be nil (no fees configured).
func OperatorFeeDefaults(g *gymDomain.Gym, member *memberDomain.Member, mt *mtDomain.MembershipType, day time.Time) FeeDefaults {
	settings := map[string]any{}
	if g != nil && g.ChargeSettings != nil {
		settings = g.ChargeSettings
	}
	var d FeeDefaults
	if settingBool(settings, "charges_enrollment") {
		d.Enrollment = effectiveFee(mt.EnrollmentFee, settingFloat(settings, "enrollment_amount"))
	}
	d.EnrollmentDue = d.Enrollment > 0 && !member.EnrollmentPaid
	if settingBool(settings, "charges_maintenance") {
		d.Maintenance = effectiveFee(mt.MaintenanceFee, settingFloat(settings, "maintenance_amount"))
	}
	frequency := ""
	if mt.MaintenanceFrequency != nil {
		frequency = *mt.MaintenanceFrequency
	}
	if frequency == "" {
		frequency, _ = settings["maintenance_frequency"].(string)
	}
	d.MaintenanceDue = d.Maintenance > 0 && maintenanceDueOn(member, frequency, day)
	return d
}

// RequireDefaultFees: an operator without PermDiscounts charges the
// enrollment and maintenance fees reception offers, for the configured
// amount, and can't skip one that is due. Charging a fee that is not due is
// allowed (it collects more, it is not a discount). The charge flags and
// amounts are the ones the use case is about to record.
func RequireDefaultFees(ctx context.Context, d FeeDefaults, chargeEnrollment bool, enrollmentAmt float64, chargeMaintenance bool, maintenanceAmt float64) error {
	if sharedDomain.RequirePermission(ctx, sharedDomain.PermDiscounts) == nil {
		return nil
	}
	denied := sharedDomain.NewPermissionDeniedError(sharedDomain.PermDiscounts)
	if d.EnrollmentDue && !chargeEnrollment {
		return denied
	}
	if chargeEnrollment && d.Enrollment > 0 && toCents(enrollmentAmt) != toCents(d.Enrollment) {
		return denied
	}
	if d.MaintenanceDue && !chargeMaintenance {
		return denied
	}
	if chargeMaintenance && d.Maintenance > 0 && toCents(maintenanceAmt) != toCents(d.Maintenance) {
		return denied
	}
	return nil
}

// RequireTodayCharge: a charge dated before the gym's today needs
// PermDiscounts (a backdated cash payment would not show in today's drawer).
func RequireTodayCharge(ctx context.Context, day, gymToday time.Time) error {
	if dateOnly(day).Before(dateOnly(gymToday)) {
		return sharedDomain.RequirePermission(ctx, sharedDomain.PermDiscounts)
	}
	return nil
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func effectiveFee(planFee, gymDefault float64) float64 {
	if planFee > 0 {
		return planFee
	}
	return gymDefault
}

// maintenanceDueOn mirrors billing's shouldChargeMaintenance with an explicit
// frequency (the plan's, or the gym default).
func maintenanceDueOn(member *memberDomain.Member, frequency string, day time.Time) bool {
	threshold := mtDomain.MaintenanceThresholdDays(frequency)
	if threshold < 0 {
		return false
	}
	if threshold == 0 || member.LastMaintenancePaid == nil {
		return true
	}
	return day.Sub(*member.LastMaintenancePaid) >= time.Duration(threshold)*24*time.Hour
}

func settingBool(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func settingFloat(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func toCents(v float64) int64 { return int64(math.Round(v * 100)) }
