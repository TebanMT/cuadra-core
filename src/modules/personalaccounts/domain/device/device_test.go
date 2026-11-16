package device

import (
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/google/uuid"
)

func TestAuthorizedDevicePreservesExactCandidateAndRevocation(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.FixedZone("fixture", -6*3600))
	candidate := attempt.Candidate{ID: uuid.New(), PublicKey: [32]byte{1}, Platform: "ios", Label: "Mi teléfono"}
	d, err := NewAuthorizedDevice(uuid.New(), candidate, Signup, now)
	if err != nil || d.ID != candidate.ID || d.PublicKey != candidate.PublicKey || d.Label != candidate.Label || d.Platform != candidate.Platform || !d.ActiveAt(now) || d.CreatedAt.Location() != time.UTC {
		t.Fatalf("candidate altered or invalid: %v", err)
	}
	candidate.PublicKey[0] = 9
	if d.PublicKey[0] != 1 {
		t.Fatal("candidate key aliased")
	}
	if err = d.Revoke(now.Add(time.Second), "user"); err != nil {
		t.Fatal(err)
	}
	first := *d.RevokedAt
	if err = d.Revoke(now.Add(2*time.Second), "recovery"); err != nil || *d.RevokedAt != first || d.RevocationReason != "user" || d.ActiveAt(now.Add(3*time.Second)) {
		t.Fatal("revocation was changed or reversed")
	}
}

func TestAuthorizedDeviceRejectsInvalidCandidateAndDeletedIsInactive(t *testing.T) {
	now := time.Now().UTC()
	valid := attempt.Candidate{ID: uuid.New(), PublicKey: [32]byte{1}, Platform: "android", Label: "Teléfono"}
	for _, mutation := range []func(*attempt.Candidate){func(c *attempt.Candidate) { c.ID = uuid.Nil }, func(c *attempt.Candidate) { c.PublicKey = [32]byte{} }, func(c *attempt.Candidate) { c.Platform = "web" }, func(c *attempt.Candidate) { c.Label = "" }, func(c *attempt.Candidate) { c.Label = "bad\x00label" }} {
		candidate := valid
		mutation(&candidate)
		if _, err := NewAuthorizedDevice(uuid.New(), candidate, Signup, now); err == nil {
			t.Fatal("invalid candidate accepted")
		}
	}
	if _, err := NewAuthorizedDevice(uuid.New(), valid, AuthorizationMethod("otp"), now); err == nil {
		t.Fatal("OTP alone accepted as authorization method")
	}
	d, _ := NewAuthorizedDevice(uuid.New(), valid, Recovery, now)
	if d.ActiveAt(now.Add(-time.Second)) {
		t.Fatal("future authorization active")
	}
	d.DeletedAt = &now
	if d.ActiveAt(now) {
		t.Fatal("deleted device active")
	}
}
