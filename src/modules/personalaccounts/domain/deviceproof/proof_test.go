package deviceproof

import (
	"reflect"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/google/uuid"
)

func proofFixture(t *testing.T) (*Proof, security.AccessClaims, time.Time) {
	t.Helper()
	actor := security.AccessClaims{AccountID: uuid.New(), SessionID: uuid.New(), DeviceID: uuid.New()}
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	p, err := New(actor, uuid.New(), security.RecoveryKey, [32]byte{1}, [32]byte{2}, now)
	if err != nil {
		t.Fatal(err)
	}
	return p, actor, now
}
func TestDeviceProofVerificationAndConsumptionKeepOriginalExpiry(t *testing.T) {
	p, actor, now := proofFixture(t)
	expiry := p.ExpiresAt
	if p.TokenHash != nil || p.VerifiedAt != nil || p.OperationID != nil || p.ConsumedAt != nil {
		t.Fatal("new proof already used")
	}
	if err := p.Verify([32]byte{3}, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !p.ExpiresAt.Equal(expiry) || p.OperationID != nil || p.ConsumedAt != nil {
		t.Fatal("verification extended or consumed proof")
	}
	if err := p.Verify([32]byte{4}, now.Add(4*time.Minute)); err == nil {
		t.Fatal("verified token overwritten")
	}
	op := uuid.New()
	if err := p.Consume(actor, security.RecoveryKey, [32]byte{1}, [32]byte{3}, op, now.Add(4*time.Minute+time.Second)); err != nil {
		t.Fatal(err)
	}
	if p.OperationID == nil || *p.OperationID != op || p.ConsumedAt == nil || !p.ExpiresAt.Equal(expiry) {
		t.Fatal("effect binding missing or expiry changed")
	}
	before := *p
	if err := p.Consume(actor, security.RecoveryKey, [32]byte{1}, [32]byte{3}, op, now.Add(4*time.Minute+2*time.Second)); err == nil || !reflect.DeepEqual(*p, before) {
		t.Fatal("proof consumption repeated")
	}
}
func TestDeviceProofRejectsActorPurposeHashAndExpiry(t *testing.T) {
	p, actor, now := proofFixture(t)
	if err := p.Verify([32]byte{3}, now); err != nil {
		t.Fatal(err)
	}
	before := *p
	for _, bad := range []security.AccessClaims{{AccountID: uuid.New(), SessionID: actor.SessionID, DeviceID: actor.DeviceID}, {AccountID: actor.AccountID, SessionID: uuid.New(), DeviceID: actor.DeviceID}, {AccountID: actor.AccountID, SessionID: actor.SessionID, DeviceID: uuid.New()}} {
		if p.Consume(bad, p.Purpose, p.TargetRequestHash, *p.TokenHash, uuid.New(), now) == nil {
			t.Fatal("different actor consumed proof")
		}
	}
	for _, check := range []func() error{func() error {
		return p.Consume(actor, security.PhoneChange, p.TargetRequestHash, *p.TokenHash, uuid.New(), now)
	}, func() error { return p.Consume(actor, p.Purpose, [32]byte{9}, *p.TokenHash, uuid.New(), now) }, func() error { return p.Consume(actor, p.Purpose, p.TargetRequestHash, [32]byte{9}, uuid.New(), now) }, func() error {
		return p.Consume(actor, p.Purpose, p.TargetRequestHash, *p.TokenHash, uuid.New(), p.ExpiresAt)
	}, func() error { return p.Consume(actor, p.Purpose, p.TargetRequestHash, *p.TokenHash, uuid.Nil, now) }} {
		if check() == nil || !reflect.DeepEqual(*p, before) {
			t.Fatal("invalid consume accepted or mutated")
		}
	}
	signed := p.SigningProof()
	signed.Nonce[0]++
	if p.Nonce[0] == signed.Nonce[0] {
		t.Fatal("signing nonce alias")
	}
	copy := *p
	copy.VerifiedAt = nil
	if copy.Validate() == nil {
		t.Fatal("token without verification accepted")
	}
	copy = *p
	op := uuid.New()
	copy.OperationID = &op
	if copy.Validate() == nil {
		t.Fatal("operation without consumption accepted")
	}
}
