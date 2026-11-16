// Package security defines personal-account cryptographic ports. Successful
// cryptographic verification does not establish an active account/session/device;
// use cases must check that state and consume proofs inside the account lock.
package security

import (
	"time"

	"github.com/google/uuid"
)

const (
	AccessTTL       = 15 * time.Minute
	AccessClockSkew = 30 * time.Second
	ProofTTL        = 5 * time.Minute
	AttemptTTL      = 10 * time.Minute
)

type AccessClaims struct {
	AccountID uuid.UUID
	SessionID uuid.UUID
	DeviceID  uuid.UUID
	IssuedAt  time.Time
	ExpiresAt time.Time
}

type AccessTokens interface {
	Issue(accountID, sessionID, deviceID uuid.UUID, now time.Time) (string, AccessClaims, error)
	Verify(token string, now time.Time) (AccessClaims, error)
}

type ProofPurpose string

const (
	AuthorizeDevice ProofPurpose = "authorize_device"
	RevokeDevices   ProofPurpose = "revoke_devices"
	RecoveryKey     ProofPurpose = "recovery_key"
	PhoneChange     ProofPurpose = "phone_change"
	DeleteAccount   ProofPurpose = "delete_account"
	VaultUnlock     ProofPurpose = "vault_unlock"
)

type DeviceProof struct {
	Purpose     ProofPurpose
	AccountID   uuid.UUID
	SessionID   uuid.UUID
	DeviceID    uuid.UUID
	ProofID     uuid.UUID
	Nonce       []byte
	RequestHash [32]byte
	ExpiresAt   time.Time
}

type ResumeProof struct {
	AttemptID uuid.UUID
	DeviceID  uuid.UUID
	Nonce     []byte
	ExpiresAt time.Time
}

type RefreshProof struct {
	OperationID uuid.UUID
	SessionID   uuid.UUID
	Generation  int64
	RefreshHash [32]byte
}

type DeviceProofs interface {
	VerifyDevice(publicKey []byte, signature string, proof DeviceProof, now time.Time) error
	VerifyResume(publicKey []byte, signature string, proof ResumeProof, now time.Time) error
	VerifyRefresh(publicKey []byte, signature string, proof RefreshProof) error
}

// Envelope stores the selected key version separately from the opaque ciphertext,
// matching the S00 key_version columns. Neither field contains the master key.
type Envelope struct {
	KeyVersion string
	Ciphertext []byte
}

type VaultContext struct {
	AccountID uuid.UUID
	VaultID   uuid.UUID
}

// A receipt may precede account creation. At least one of AccountID/AttemptID
// must be set; a zero UUID denotes the absent context, serialized as JSON null.
type ReceiptContext struct {
	AccountID   uuid.UUID
	AttemptID   uuid.UUID
	OperationID uuid.UUID
	Purpose     string
}

type Envelopes interface {
	WrapVault(context VaultContext, dataKey []byte) (Envelope, error)
	UnwrapVault(context VaultContext, envelope Envelope) ([]byte, error)
	SealReceipt(context ReceiptContext, plaintext []byte) (Envelope, error)
	OpenReceipt(context ReceiptContext, envelope Envelope) ([]byte, error)
}

type RecoveryVerifier struct {
	KeyVersion string
	Digest     []byte
}

type RecoveryKeys interface {
	CreateVerifier(secret string) (RecoveryVerifier, error)
	Verify(secret string, verifier RecoveryVerifier) error
}
