package deviceproof

import (
	"crypto/subtle"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/google/uuid"
)

type Proof struct {
	ID, AccountID, SessionID, DeviceID uuid.UUID
	Purpose                            security.ProofPurpose
	Nonce, TargetRequestHash           [32]byte
	ExpiresAt                          time.Time
	TokenHash                          *[32]byte
	VerifiedAt, ConsumedAt             *time.Time
	OperationID                        *uuid.UUID
}

func New(actor security.AccessClaims, id uuid.UUID, purpose security.ProofPurpose, targetHash, nonce [32]byte, now time.Time) (*Proof, error) {
	if now.IsZero() {
		return nil, personalErrors.ErrInvalidProof
	}
	p := &Proof{ID: id, AccountID: actor.AccountID, SessionID: actor.SessionID, DeviceID: actor.DeviceID, Purpose: purpose, Nonce: nonce, TargetRequestHash: targetHash, ExpiresAt: now.UTC().Truncate(time.Millisecond).Add(security.ProofTTL)}
	return p, p.Validate()
}
func (p *Proof) Validate() error {
	if p == nil || p.Nonce == [32]byte{} || p.TargetRequestHash == [32]byte{} || p.ExpiresAt.IsZero() || p.ExpiresAt.Nanosecond()%int(time.Millisecond) != 0 {
		return personalErrors.ErrInvalidProof
	}
	for _, id := range []uuid.UUID{p.ID, p.AccountID, p.SessionID, p.DeviceID} {
		if id == uuid.Nil || id.Variant() != uuid.RFC4122 || id.Version() < 1 || id.Version() > 8 {
			return personalErrors.ErrInvalidProof
		}
	}
	switch p.Purpose {
	case security.AuthorizeDevice, security.RevokeDevices, security.RecoveryKey, security.PhoneChange, security.DeleteAccount, security.VaultUnlock:
	default:
		return personalErrors.ErrInvalidProof
	}
	if (p.TokenHash == nil) != (p.VerifiedAt == nil) || (p.ConsumedAt == nil) != (p.OperationID == nil) {
		return personalErrors.ErrInvalidProof
	}
	if p.TokenHash != nil && (*p.TokenHash == [32]byte{} || p.VerifiedAt.Before(p.ExpiresAt.Add(-security.ProofTTL)) || !p.VerifiedAt.Before(p.ExpiresAt)) {
		return personalErrors.ErrInvalidProof
	}
	if p.ConsumedAt != nil && (p.VerifiedAt == nil || *p.OperationID == uuid.Nil || p.ConsumedAt.Before(*p.VerifiedAt) || !p.ConsumedAt.Before(p.ExpiresAt)) {
		return personalErrors.ErrInvalidProof
	}
	return nil
}
func (p *Proof) MatchesActor(actor security.AccessClaims) bool {
	return p != nil && p.AccountID == actor.AccountID && p.SessionID == actor.SessionID && p.DeviceID == actor.DeviceID
}
func (p *Proof) ValidAt(now time.Time) bool {
	return p.Validate() == nil && !now.IsZero() && !now.Before(p.ExpiresAt.Add(-security.ProofTTL)) && now.Before(p.ExpiresAt)
}
func (p *Proof) Verify(tokenHash [32]byte, now time.Time) error {
	if !p.ValidAt(now) || p.TokenHash != nil || p.ConsumedAt != nil || tokenHash == [32]byte{} {
		return personalErrors.ErrInvalidProof
	}
	at := now.UTC().Truncate(time.Millisecond)
	copy := tokenHash
	p.TokenHash, p.VerifiedAt = &copy, &at
	return nil
}
func (p *Proof) Consume(actor security.AccessClaims, purpose security.ProofPurpose, targetHash, tokenHash [32]byte, operationID uuid.UUID, now time.Time) error {
	if !p.ValidAt(now) || !p.MatchesActor(actor) || p.Purpose != purpose || p.TokenHash == nil || p.ConsumedAt != nil || operationID == uuid.Nil || now.Before(*p.VerifiedAt) ||
		subtle.ConstantTimeCompare(p.TargetRequestHash[:], targetHash[:]) != 1 || subtle.ConstantTimeCompare(p.TokenHash[:], tokenHash[:]) != 1 {
		return personalErrors.ErrInvalidProof
	}
	at := now.UTC().Truncate(time.Millisecond)
	op := operationID
	p.ConsumedAt, p.OperationID = &at, &op
	return nil
}
func (p *Proof) SigningProof() security.DeviceProof {
	return security.DeviceProof{Purpose: p.Purpose, AccountID: p.AccountID, SessionID: p.SessionID, DeviceID: p.DeviceID, ProofID: p.ID, Nonce: append([]byte(nil), p.Nonce[:]...), RequestHash: p.TargetRequestHash, ExpiresAt: p.ExpiresAt}
}
func (Proof) String() string     { return "personal device proof (nonce and token hash redacted)" }
func (p Proof) GoString() string { return p.String() }
