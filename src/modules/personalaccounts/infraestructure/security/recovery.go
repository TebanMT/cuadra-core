package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"io"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	personalSecurity "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
)

type RecoveryKeys struct {
	activeKeyVersion string
	keys             map[string][]byte
}

var _ personalSecurity.RecoveryKeys = (*RecoveryKeys)(nil)

// Recovery verification material must be configured independently of JWT and
// vault encryption keys. Old versions remain available until their kits retire.
func NewRecoveryKeys(activeKeyVersion string, keys map[string][]byte) (*RecoveryKeys, error) {
	copied, err := copySymmetricKeys(activeKeyVersion, keys)
	if err != nil {
		return nil, err
	}
	return &RecoveryKeys{activeKeyVersion: activeKeyVersion, keys: copied}, nil
}

func NewRecoverySecret() (string, error) {
	secret := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return "", err
	}
	return rawURL.EncodeToString(secret), nil
}

func (s *RecoveryKeys) CreateVerifier(secret string) (personalSecurity.RecoveryVerifier, error) {
	if !validRecoverySecret(secret) {
		return personalSecurity.RecoveryVerifier{}, personalErrors.ErrInvalidRecoveryKey
	}
	return personalSecurity.RecoveryVerifier{KeyVersion: s.activeKeyVersion, Digest: recoveryDigest(s.keys[s.activeKeyVersion], secret)}, nil
}

func (s *RecoveryKeys) Verify(secret string, verifier personalSecurity.RecoveryVerifier) error {
	key, found := s.keys[verifier.KeyVersion]
	if !found || !validRecoverySecret(secret) || len(verifier.Digest) != sha256.Size {
		return personalErrors.ErrInvalidRecoveryKey
	}
	if !hmac.Equal(recoveryDigest(key, secret), verifier.Digest) {
		return personalErrors.ErrInvalidRecoveryKey
	}
	return nil
}

func validRecoverySecret(secret string) bool {
	if len(secret) != 43 {
		return false
	}
	decoded, err := rawURL.DecodeString(secret)
	return err == nil && len(decoded) == 32
}

// The verifier covers the exact canonical base64url secret supplied by the
// client, without its separate kit ID. Account/kit ownership and one-time use
// are persisted domain checks, not permissions conveyed by the secret's hash.
func recoveryDigest(key []byte, secret string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(secret))
	return mac.Sum(nil)
}
