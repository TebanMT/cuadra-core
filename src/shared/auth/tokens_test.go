package auth_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/shared/auth"
)

func TestJWTRoundTrip(t *testing.T) {
	svc := auth.NewJWTService("test-secret-do-not-reuse-please")
	uid := uuid.New()
	gid := uuid.New()
	access, err := svc.GenerateAccessToken(uid, gid, "owner")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	c, err := svc.ValidateAccessToken(access)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.UserID != uid || c.GymID != gid || c.Role != "owner" {
		t.Errorf("claims mismatch: %+v", c)
	}
	// Wrong type should fail.
	if _, err := svc.ValidateRefreshToken(access); err == nil {
		t.Errorf("access token should not validate as refresh")
	}
}

func TestPasswordHash(t *testing.T) {
	hash, err := auth.HashPassword("hunter2sufficient")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := auth.VerifyPassword(hash, "hunter2sufficient"); err != nil {
		t.Errorf("verify ok password: %v", err)
	}
	if err := auth.VerifyPassword(hash, "wrong"); err == nil {
		t.Errorf("verify wrong password should fail")
	}
}

func TestGenerateTempPassword(t *testing.T) {
	p, err := auth.GenerateTempPassword()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(p) != 6 {
		t.Errorf("len = %d, want 6", len(p))
	}
	for _, c := range p {
		if c == '0' || c == 'O' || c == 'l' || c == '1' || c == 'I' {
			t.Errorf("forbidden char in %q", p)
		}
	}
}

func TestGenerateOTPCode(t *testing.T) {
	code, err := auth.GenerateOTPCode(6)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(code) != 6 {
		t.Errorf("len = %d", len(code))
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			t.Errorf("non-digit in %q", code)
		}
	}
}
