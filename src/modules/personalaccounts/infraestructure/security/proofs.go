package security

import (
	"crypto/ed25519"
	"encoding/hex"
	"strconv"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	personalSecurity "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
)

type DeviceProofs struct{}

var _ personalSecurity.DeviceProofs = DeviceProofs{}

func DeviceProofPayload(proof personalSecurity.DeviceProof) ([]byte, error) {
	if !validID(proof.AccountID) || !validID(proof.SessionID) || !validID(proof.DeviceID) || !validID(proof.ProofID) || len(proof.Nonce) != 32 {
		return nil, personalErrors.ErrInvalidProof
	}
	switch proof.Purpose {
	case personalSecurity.AuthorizeDevice, personalSecurity.RevokeDevices, personalSecurity.RecoveryKey, personalSecurity.PhoneChange, personalSecurity.DeleteAccount, personalSecurity.VaultUnlock:
	default:
		return nil, personalErrors.ErrInvalidProof
	}
	expiresAt, err := proofTimestamp(proof.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return canonical(map[string]any{
		"v": 1, "purpose": string(proof.Purpose), "account_id": proof.AccountID.String(), "session_id": proof.SessionID.String(),
		"device_id": proof.DeviceID.String(), "proof_id": proof.ProofID.String(), "nonce": rawURL.EncodeToString(proof.Nonce),
		"request_hash": hex.EncodeToString(proof.RequestHash[:]), "expires_at": expiresAt,
	})
}

func ResumeProofPayload(proof personalSecurity.ResumeProof) ([]byte, error) {
	if !validID(proof.AttemptID) || !validID(proof.DeviceID) || len(proof.Nonce) != 32 {
		return nil, personalErrors.ErrInvalidProof
	}
	expiresAt, err := proofTimestamp(proof.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return canonical(map[string]any{"v": 1, "purpose": "resume_device", "attempt_id": proof.AttemptID.String(), "device_id": proof.DeviceID.String(), "nonce": rawURL.EncodeToString(proof.Nonce), "expires_at": expiresAt})
}

func RefreshProofPayload(proof personalSecurity.RefreshProof) ([]byte, error) {
	if !validID(proof.OperationID) || !validID(proof.SessionID) || proof.Generation < 1 {
		return nil, personalErrors.ErrInvalidProof
	}
	return canonical(map[string]any{"v": 1, "purpose": "refresh", "operation_id": proof.OperationID.String(), "session_id": proof.SessionID.String(), "generation": strconv.FormatInt(proof.Generation, 10), "refresh_hash": hex.EncodeToString(proof.RefreshHash[:])})
}

func (DeviceProofs) VerifyDevice(publicKey []byte, signature string, proof personalSecurity.DeviceProof, now time.Time) error {
	if !validExpiry(proof.ExpiresAt, now, personalSecurity.ProofTTL) {
		return personalErrors.ErrInvalidProof
	}
	payload, err := DeviceProofPayload(proof)
	if err != nil {
		return err
	}
	return verifySignature(publicKey, signature, payload)
}

func (DeviceProofs) VerifyResume(publicKey []byte, signature string, proof personalSecurity.ResumeProof, now time.Time) error {
	if !validExpiry(proof.ExpiresAt, now, personalSecurity.AttemptTTL) {
		return personalErrors.ErrInvalidProof
	}
	payload, err := ResumeProofPayload(proof)
	if err != nil {
		return err
	}
	return verifySignature(publicKey, signature, payload)
}

func (DeviceProofs) VerifyRefresh(publicKey []byte, signature string, proof personalSecurity.RefreshProof) error {
	payload, err := RefreshProofPayload(proof)
	if err != nil {
		return err
	}
	return verifySignature(publicKey, signature, payload)
}

func verifySignature(publicKey []byte, signature string, payload []byte) error {
	if len(publicKey) != ed25519.PublicKeySize || len(signature) != 86 {
		return personalErrors.ErrInvalidProof
	}
	decoded, err := rawURL.DecodeString(signature)
	if err != nil || len(decoded) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(publicKey), payload, decoded) {
		return personalErrors.ErrInvalidProof
	}
	return nil
}

func validExpiry(expiresAt, now time.Time, maximum time.Duration) bool {
	return !now.IsZero() && expiresAt.After(now) && !expiresAt.After(now.Add(maximum))
}

// S00 permits seconds or three decimals. Signed payloads and public issuance
// normalize to exactly three UTC decimals, including .000Z, so the value the
// client receives and signs is identical at a whole-second boundary too.
func proofTimestamp(at time.Time) (string, error) {
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 || at.Nanosecond()%int(time.Millisecond) != 0 {
		return "", personalErrors.ErrInvalidProof
	}
	return at.UTC().Format("2006-01-02T15:04:05.000Z"), nil
}
