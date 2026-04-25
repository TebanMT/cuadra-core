package member_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/members/domain/member"
)

func mustNewMember(t *testing.T) *member.Member {
	t.Helper()
	now := time.Now().UTC()
	m, err := member.NewMember(uuid.New(), uuid.New(), "MEM-000001",
		"Juan Pérez", "+524421234567", uuid.New(), now)
	if err != nil {
		t.Fatalf("NewMember: %v", err)
	}
	return m
}

func TestNewMember(t *testing.T) {
	now := time.Now().UTC()
	gymID := uuid.New()
	createdBy := uuid.New()

	if _, err := member.NewMember(uuid.New(), gymID, "MEM-1", "Jo", "+524421234567", createdBy, now); err == nil {
		t.Errorf("name too short should fail (<3 chars)")
	}
	if _, err := member.NewMember(uuid.New(), gymID, "MEM-1", "Juan Pérez", "abc", createdBy, now); err == nil {
		t.Errorf("invalid phone should fail")
	}
	if _, err := member.NewMember(uuid.New(), gymID, "MEM-1", "Juan Pérez",
		strings.Repeat("9", 16), createdBy, now); err == nil {
		t.Errorf("phone too long should fail")
	}
	m, err := member.NewMember(uuid.New(), gymID, "MEM-1", "Juan Pérez", "+524421234567", createdBy, now)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if m.Status != member.StatusActive {
		t.Errorf("default status = %q, want %q", m.Status, member.StatusActive)
	}
	if m.Version != 1 {
		t.Errorf("default version = %d, want 1", m.Version)
	}
}

func TestApplyProfileUpdate(t *testing.T) {
	m := mustNewMember(t)
	now := time.Now().UTC()
	v0 := m.Version
	email := "juan@example.com"
	notes := "VIP"
	if err := m.ApplyProfileUpdate(member.ProfileUpdate{
		Email: &email, Notes: &notes,
	}, now); err != nil {
		t.Fatalf("update: %v", err)
	}
	if m.Email == nil || *m.Email != email {
		t.Errorf("email = %v", m.Email)
	}
	if m.Version != v0+1 {
		t.Errorf("version = %d, want %d", m.Version, v0+1)
	}
	bad := "not-an-email"
	if err := m.ApplyProfileUpdate(member.ProfileUpdate{Email: &bad}, now); err == nil {
		t.Errorf("bad email should fail")
	}
}

func TestChangeStatus(t *testing.T) {
	m := mustNewMember(t)
	now := time.Now().UTC()
	if err := m.ChangeStatus("inactive", now); err != nil {
		t.Fatalf("active->inactive: %v", err)
	}
	if m.Status != member.StatusInactive {
		t.Errorf("status = %q", m.Status)
	}
	// Unknown status should fail.
	if err := m.ChangeStatus("zombie", now); err == nil {
		t.Errorf("unknown status should fail")
	}
	// Idempotent: same status should NOT bump version.
	v := m.Version
	if err := m.ChangeStatus("inactive", now); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	if m.Version != v {
		t.Errorf("idempotent change bumped version: %d -> %d", v, m.Version)
	}
}

func TestValidatePin(t *testing.T) {
	if err := member.ValidatePin("1234"); err != nil {
		t.Errorf("4-digit pin should pass")
	}
	if err := member.ValidatePin("12a4"); err == nil {
		t.Errorf("non-digit should fail")
	}
	if err := member.ValidatePin("123"); err == nil {
		t.Errorf("3-digit should fail")
	}
	if err := member.ValidatePin("12345"); err == nil {
		t.Errorf("5-digit should fail")
	}
}

func TestValidateStartDate(t *testing.T) {
	today := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	if err := member.ValidateStartDate(today, today); err != nil {
		t.Errorf("today should be valid")
	}
	if err := member.ValidateStartDate(today.AddDate(0, 0, -10), today); err != nil {
		t.Errorf("10 days ago should be valid")
	}
	if err := member.ValidateStartDate(today.AddDate(0, 0, 60), today); err == nil {
		t.Errorf("60 days ahead should be invalid")
	}
	if err := member.ValidateStartDate(today.AddDate(0, 0, -60), today); err == nil {
		t.Errorf("60 days ago should be invalid")
	}
}

func TestValidatorChain(t *testing.T) {
	m := mustNewMember(t)
	if err := member.BuildValidatorChain().Validate(m); err != nil {
		t.Errorf("happy path validator chain: %v", err)
	}
	bad := mustNewMember(t)
	bad.Status = "wrong"
	if err := member.BuildValidatorChain().Validate(bad); err == nil {
		t.Errorf("bad status should fail")
	}
	if err := member.BuildValidatorChain().Validate(bad); err != nil {
		// Make sure we hit the status validator branch (not an earlier one).
		if !errorIs(err, memErrors.ErrInvalidStatus) {
			t.Errorf("got %v, want ErrInvalidStatus", err)
		}
	}
}

func errorIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
