package app

import (
	"context"
	"errors"
	"testing"
	"time"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func feeGym(settings map[string]any) *gymDomain.Gym { return &gymDomain.Gym{ChargeSettings: settings} }

func TestOperatorFeeDefaults_FollowsReception(t *testing.T) {
	day := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	monthly := mtDomain.FrequencyMonthly
	quarterly := mtDomain.FrequencyQuarterly
	paidRecently := day.AddDate(0, 0, -30)
	newMember := &memberDomain.Member{}

	cases := []struct {
		name     string
		gym      *gymDomain.Gym
		member   *memberDomain.Member
		mt       *mtDomain.MembershipType
		want     FeeDefaults
		wantNote string
	}{
		{
			name: "gym does not charge fees",
			gym:  feeGym(map[string]any{}), member: newMember,
			mt: &mtDomain.MembershipType{EnrollmentFee: 100, MaintenanceFee: 50, MaintenanceFrequency: &monthly},
		},
		{
			name:   "plan fee wins over the gym default",
			gym:    feeGym(map[string]any{"charges_enrollment": true, "enrollment_amount": 150.0}),
			member: newMember, mt: &mtDomain.MembershipType{EnrollmentFee: 100},
			want: FeeDefaults{Enrollment: 100, EnrollmentDue: true},
		},
		{
			name:   "gym default when the plan has none",
			gym:    feeGym(map[string]any{"charges_enrollment": true, "enrollment_amount": 150.0}),
			member: &memberDomain.Member{EnrollmentPaid: true}, mt: &mtDomain.MembershipType{},
			want: FeeDefaults{Enrollment: 150},
		},
		{
			name: "maintenance uses the gym frequency when the plan has none",
			gym: feeGym(map[string]any{
				"charges_maintenance": true, "maintenance_amount": 80.0, "maintenance_frequency": quarterly,
			}),
			member: &memberDomain.Member{LastMaintenancePaid: &paidRecently}, mt: &mtDomain.MembershipType{},
			want: FeeDefaults{Maintenance: 80},
		},
		{
			name:   "maintenance without any frequency is never due",
			gym:    feeGym(map[string]any{"charges_maintenance": true, "maintenance_amount": 80.0}),
			member: newMember, mt: &mtDomain.MembershipType{},
			want: FeeDefaults{Maintenance: 80},
		},
		{
			name:   "monthly maintenance is always due",
			gym:    feeGym(map[string]any{"charges_maintenance": true}),
			member: &memberDomain.Member{LastMaintenancePaid: &paidRecently},
			mt:     &mtDomain.MembershipType{MaintenanceFee: 60, MaintenanceFrequency: &monthly},
			want:   FeeDefaults{Maintenance: 60, MaintenanceDue: true},
		},
	}
	for _, c := range cases {
		if got := OperatorFeeDefaults(c.gym, c.member, c.mt, day); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	if got := OperatorFeeDefaults(nil, newMember, &mtDomain.MembershipType{EnrollmentFee: 100}, day); got != (FeeDefaults{}) {
		t.Errorf("nil gym: %+v", got)
	}
}

func TestRequireDefaultFees(t *testing.T) {
	operator := sharedDomain.WithGrants(context.Background(), sharedDomain.OperatorGrants(nil))
	withDiscounts := sharedDomain.WithGrants(context.Background(),
		sharedDomain.OperatorGrants([]sharedDomain.Permission{sharedDomain.PermDiscounts}))
	due := FeeDefaults{Enrollment: 100, EnrollmentDue: true, Maintenance: 50, MaintenanceDue: true}
	notDue := FeeDefaults{Enrollment: 100, Maintenance: 50}

	type charge struct {
		enroll    bool
		enrollAmt float64
		maint     bool
		maintAmt  float64
	}
	cases := []struct {
		name   string
		d      FeeDefaults
		c      charge
		denied bool
	}{
		{"charges what reception offers", due, charge{true, 100, true, 50}, false},
		{"skips a due enrollment", due, charge{false, 0, true, 50}, true},
		{"skips a due maintenance", due, charge{true, 100, false, 0}, true},
		{"charges a lower enrollment", due, charge{true, 40, true, 50}, true},
		{"charges a different maintenance", due, charge{true, 100, true, 70}, true},
		{"skips fees that are not due", notDue, charge{false, 0, false, 0}, false},
		{"forces a fee that is not due", notDue, charge{true, 100, true, 50}, false},
		{"gym does not charge it", FeeDefaults{}, charge{true, 999, false, 0}, false},
	}
	for _, c := range cases {
		err := RequireDefaultFees(operator, c.d, c.c.enroll, c.c.enrollAmt, c.c.maint, c.c.maintAmt)
		if got := errors.Is(err, sharedDomain.ErrPermissionDenied); got != c.denied {
			t.Errorf("%s: denied = %v (err %v), want %v", c.name, got, err, c.denied)
		}
		if err := RequireDefaultFees(withDiscounts, c.d, c.c.enroll, c.c.enrollAmt, c.c.maint, c.c.maintAmt); err != nil {
			t.Errorf("%s with discounts: %v", c.name, err)
		}
	}
}

func TestRequireTodayCharge(t *testing.T) {
	operator := sharedDomain.WithGrants(context.Background(), sharedDomain.OperatorGrants(nil))
	today := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	if err := RequireTodayCharge(operator, today.Add(20*time.Hour), today); err != nil {
		t.Errorf("today: %v", err)
	}
	if err := RequireTodayCharge(operator, today.AddDate(0, 0, -1), today); !errors.Is(err, sharedDomain.ErrPermissionDenied) {
		t.Errorf("yesterday: err = %v, want denied", err)
	}
	if err := RequireTodayCharge(context.Background(), today.AddDate(0, 0, -1), today); err != nil {
		t.Errorf("yesterday without grants: %v", err)
	}
}
