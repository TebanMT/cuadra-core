package checkin_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/checkins/domain/checkin"
	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
)

func TestNewFingerprintCheckin_MapsAccessStatusToResult(t *testing.T) {
	cases := []struct {
		name   string
		status access.AccessStatus
		want   string
	}{
		{"active", access.AllowedActive, checkin.ResultAllowedActive},
		{"expiring_soon", access.AllowedExpiringSoon, checkin.ResultAllowedExpiringSoon},
		{"expired", access.DeniedExpired, checkin.ResultDeniedExpired},
		{"inactive", access.DeniedInactive, checkin.ResultDeniedInactive},
		{"no_membership", access.DeniedNoMembership, checkin.ResultDeniedNoMembership},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := checkin.NewFingerprintCheckin(uuid.New(), uuid.New(), uuid.New(), tc.status, time.Now())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if c.Result != tc.want {
				t.Errorf("got %s, want %s", c.Result, tc.want)
			}
			if c.Method != checkin.MethodFingerprint {
				t.Errorf("method should be fingerprint, got %s", c.Method)
			}
			if c.OperatorID != nil {
				t.Errorf("automatic checkin should not have an operator")
			}
			if c.ManualOverride {
				t.Errorf("automatic checkin should not be manual_override")
			}
		})
	}
}

func TestNewManualCheckin_RequiresOperator(t *testing.T) {
	_, err := checkin.NewManualCheckin(uuid.New(), uuid.New(), uuid.New(), uuid.Nil, access.AllowedActive, time.Now())
	if !errors.Is(err, chkErrors.ErrOperatorRequired) {
		t.Errorf("expected ErrOperatorRequired, got %v", err)
	}
	c, err := checkin.NewManualCheckin(uuid.New(), uuid.New(), uuid.New(), uuid.New(), access.AllowedActive, time.Now())
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if c.OperatorID == nil {
		t.Errorf("manual checkin must record operator")
	}
}

func TestNewOverrideCheckin_ValidatesReason(t *testing.T) {
	gym, member, op := uuid.New(), uuid.New(), uuid.New()
	now := time.Now()

	if _, err := checkin.NewOverrideCheckin(uuid.New(), gym, member, op, checkin.MethodFingerprint, "ok", now); !errors.Is(err, chkErrors.ErrOverrideReasonTooShort) {
		t.Errorf("short reason should fail, got %v", err)
	}
	if _, err := checkin.NewOverrideCheckin(uuid.New(), gym, member, op, "noexiste", "razón válida", now); !errors.Is(err, chkErrors.ErrInvalidMethod) {
		t.Errorf("unknown method should fail, got %v", err)
	}
	c, err := checkin.NewOverrideCheckin(uuid.New(), gym, member, op, checkin.MethodFingerprint, "olvidó tarjeta", now)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if !c.ManualOverride {
		t.Errorf("override should set ManualOverride=true")
	}
	if c.Result != checkin.ResultAllowedOverride {
		t.Errorf("override result should be allowed_override, got %s", c.Result)
	}
	if c.OverrideReason == nil || *c.OverrideReason != "olvidó tarjeta" {
		t.Errorf("override reason not persisted: %+v", c.OverrideReason)
	}
	if c.Method != checkin.MethodFingerprint {
		t.Errorf("override should preserve original method, got %s", c.Method)
	}
}

func TestIsAllowed(t *testing.T) {
	allowed := []string{
		checkin.ResultAllowedActive,
		checkin.ResultAllowedExpiringSoon,
		checkin.ResultAllowedOverride,
	}
	denied := []string{
		checkin.ResultDeniedExpired,
		checkin.ResultDeniedInactive,
		checkin.ResultDeniedNoMembership,
	}
	for _, r := range allowed {
		c := &checkin.Checkin{Result: r}
		if !c.IsAllowed() {
			t.Errorf("%s should be allowed", r)
		}
	}
	for _, r := range denied {
		c := &checkin.Checkin{Result: r}
		if c.IsAllowed() {
			t.Errorf("%s should NOT be allowed", r)
		}
	}
}
