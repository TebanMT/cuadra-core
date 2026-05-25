package access_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	"github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
)

func makeMemberAndMembership(t *testing.T, status string, expiryOffset int) (*memberDomain.Member, *membership.Membership, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	gymID := uuid.New()

	m, err := memberDomain.NewMember(uuid.New(), gymID, "MEM-1", "Juan Pérez", "+524421234567", uuid.New(), now)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	m.Status = status

	mt, _ := mtDomain.New(uuid.New(), gymID, "Mensual", 500, 30, nil, 0, 0, "", now)
	start := today.AddDate(0, 0, expiryOffset-30)
	ms := membership.New(uuid.New(), gymID, m.ID, mt, start, now)
	// Ensure expiry == today + offset.
	exp := today.AddDate(0, 0, expiryOffset)
	ms.ExpiryDate = &exp
	return m, ms, today
}

// makePendingMember builds a member + pending_payment membership pair —
// se inscribió pero no ha pagado todavía.
func makePendingMember(t *testing.T) (*memberDomain.Member, *membership.Membership, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	gymID := uuid.New()
	m, err := memberDomain.NewMember(uuid.New(), gymID, "MEM-2", "Pedro Soto", "+524429876543", uuid.New(), now)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	mt, _ := mtDomain.New(uuid.New(), gymID, "Mensual", 500, 30, nil, 0, 0, "", now)
	ms := membership.NewPendingPayment(uuid.New(), gymID, m.ID, mt, today, now)
	return m, ms, today
}

func TestEvaluator_PendingPayment(t *testing.T) {
	m, ms, today := makePendingMember(t)
	if got := access.New().Evaluate(m, ms, today); got != access.DeniedUnpaidEnrollment {
		t.Errorf("pending_payment should be denied_unpaid_enrollment, got %s", got)
	}
}

func TestEvaluator_NilMember(t *testing.T) {
	if got := access.New().Evaluate(nil, nil, time.Now()); got != access.DeniedNoMembership {
		t.Errorf("got %s", got)
	}
}

func TestEvaluator_InactiveMember(t *testing.T) {
	m, ms, today := makeMemberAndMembership(t, memberDomain.StatusInactive, 30)
	if got := access.New().Evaluate(m, ms, today); got != access.DeniedInactive {
		t.Errorf("got %s, want denied_inactive", got)
	}
}

func TestEvaluator_NoMembership(t *testing.T) {
	m, _, today := makeMemberAndMembership(t, memberDomain.StatusActive, 30)
	if got := access.New().Evaluate(m, nil, today); got != access.DeniedNoMembership {
		t.Errorf("got %s, want denied_no_membership", got)
	}
}

func TestEvaluator_AllowedActive(t *testing.T) {
	m, ms, today := makeMemberAndMembership(t, memberDomain.StatusActive, 20)
	if got := access.New().Evaluate(m, ms, today); got != access.AllowedActive {
		t.Errorf("got %s, want allowed_active (20 days left)", got)
	}
}

func TestEvaluator_ExpiringSoon(t *testing.T) {
	m, ms, today := makeMemberAndMembership(t, memberDomain.StatusActive, 5)
	if got := access.New().Evaluate(m, ms, today); got != access.AllowedExpiringSoon {
		t.Errorf("got %s, want allowed_expiring_soon (5 days left)", got)
	}
}

func TestEvaluator_AtThreshold(t *testing.T) {
	m, ms, today := makeMemberAndMembership(t, memberDomain.StatusActive, 7)
	if got := access.New().Evaluate(m, ms, today); got != access.AllowedExpiringSoon {
		t.Errorf("at threshold (7d) should be expiring_soon, got %s", got)
	}
}

func TestEvaluator_AtThresholdPlusOne(t *testing.T) {
	m, ms, today := makeMemberAndMembership(t, memberDomain.StatusActive, 8)
	if got := access.New().Evaluate(m, ms, today); got != access.AllowedActive {
		t.Errorf("at 8d should be allowed_active, got %s", got)
	}
}

func TestEvaluator_DeniedExpired(t *testing.T) {
	m, ms, today := makeMemberAndMembership(t, memberDomain.StatusActive, -3)
	if got := access.New().Evaluate(m, ms, today); got != access.DeniedExpired {
		t.Errorf("got %s, want denied_expired", got)
	}
}

func TestEvaluator_ReplacedMembership(t *testing.T) {
	m, ms, today := makeMemberAndMembership(t, memberDomain.StatusActive, 30)
	ms.Status = membership.StatusReplaced
	if got := access.New().Evaluate(m, ms, today); got != access.DeniedNoMembership {
		t.Errorf("replaced membership should be denied_no_membership, got %s", got)
	}
}
