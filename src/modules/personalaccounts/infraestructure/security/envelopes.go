package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	personalSecurity "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/google/uuid"
)

const envelopeVersion byte = 1

type Envelopes struct {
	activeKeyVersion string
	keys             map[string][]byte
}

var _ personalSecurity.Envelopes = (*Envelopes)(nil)

func NewEnvelopes(activeKeyVersion string, keys map[string][]byte) (*Envelopes, error) {
	copied, err := copySymmetricKeys(activeKeyVersion, keys)
	if err != nil {
		return nil, err
	}
	return &Envelopes{activeKeyVersion: activeKeyVersion, keys: copied}, nil
}

func copySymmetricKeys(active string, keys map[string][]byte) (map[string][]byte, error) {
	if !validKeyVersion(active) || len(keys[active]) != 32 {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	copied := make(map[string][]byte, len(keys))
	for version, key := range keys {
		if !validKeyVersion(version) || len(key) != 32 {
			return nil, personalErrors.ErrInvalidSecurityConfiguration
		}
		copied[version] = append([]byte(nil), key...)
	}
	return copied, nil
}

func (s *Envelopes) WrapVault(context personalSecurity.VaultContext, dataKey []byte) (personalSecurity.Envelope, error) {
	if len(dataKey) != 32 {
		return personalSecurity.Envelope{}, personalErrors.ErrInvalidEnvelope
	}
	aad, err := vaultAAD(context, s.activeKeyVersion)
	if err != nil {
		return personalSecurity.Envelope{}, err
	}
	return s.seal(dataKey, aad)
}

func (s *Envelopes) UnwrapVault(context personalSecurity.VaultContext, envelope personalSecurity.Envelope) ([]byte, error) {
	aad, err := vaultAAD(context, envelope.KeyVersion)
	if err != nil {
		return nil, err
	}
	key, err := s.open(envelope, aad)
	if err != nil || len(key) != 32 {
		return nil, personalErrors.ErrInvalidEnvelope
	}
	return key, nil
}

// The caller enforces the receipt's persistence/expiration window and deletes
// expired ciphertext. Encryption alone does not authorize reading a receipt.
func (s *Envelopes) SealReceipt(context personalSecurity.ReceiptContext, plaintext []byte) (personalSecurity.Envelope, error) {
	aad, err := receiptAAD(context, s.activeKeyVersion)
	if err != nil {
		return personalSecurity.Envelope{}, err
	}
	return s.seal(plaintext, aad)
}

func (s *Envelopes) OpenReceipt(context personalSecurity.ReceiptContext, envelope personalSecurity.Envelope) ([]byte, error) {
	aad, err := receiptAAD(context, envelope.KeyVersion)
	if err != nil {
		return nil, err
	}
	return s.open(envelope, aad)
}

func vaultAAD(context personalSecurity.VaultContext, version string) ([]byte, error) {
	if !validID(context.AccountID) || !validID(context.VaultID) || !validKeyVersion(version) {
		return nil, personalErrors.ErrInvalidEnvelope
	}
	return canonical(map[string]any{"account_id": context.AccountID.String(), "vault_id": context.VaultID.String(), "key_version": version})
}

func receiptAAD(context personalSecurity.ReceiptContext, version string) ([]byte, error) {
	if (context.AccountID == uuid.Nil && context.AttemptID == uuid.Nil) ||
		(context.AccountID != uuid.Nil && !validID(context.AccountID)) || (context.AttemptID != uuid.Nil && !validID(context.AttemptID)) ||
		!validID(context.OperationID) || context.Purpose == "" || !validKeyVersion(version) {
		return nil, personalErrors.ErrInvalidEnvelope
	}
	var accountID, attemptID any
	if context.AccountID != uuid.Nil {
		accountID = context.AccountID.String()
	}
	if context.AttemptID != uuid.Nil {
		attemptID = context.AttemptID.String()
	}
	return canonical(map[string]any{"account_id": accountID, "attempt_id": attemptID, "operation_id": context.OperationID.String(), "purpose": context.Purpose, "key_version": version})
}

// Ciphertext format v1 is [version byte][96-bit random nonce][ciphertext+tag].
// The version is checked before opening; key version is authenticated in AAD.
func (s *Envelopes) seal(plaintext, aad []byte) (personalSecurity.Envelope, error) {
	aead, err := newGCM(s.keys[s.activeKeyVersion])
	if err != nil {
		return personalSecurity.Envelope{}, err
	}
	data := make([]byte, 1+aead.NonceSize())
	data[0] = envelopeVersion
	if _, err := io.ReadFull(rand.Reader, data[1:]); err != nil {
		return personalSecurity.Envelope{}, err
	}
	nonce := data[1:]
	data = aead.Seal(data, nonce, plaintext, aad)
	return personalSecurity.Envelope{KeyVersion: s.activeKeyVersion, Ciphertext: data}, nil
}

func (s *Envelopes) open(envelope personalSecurity.Envelope, aad []byte) ([]byte, error) {
	key, found := s.keys[envelope.KeyVersion]
	if !found {
		return nil, personalErrors.ErrInvalidEnvelope
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	data := envelope.Ciphertext
	if len(data) < 1+aead.NonceSize()+aead.Overhead() || data[0] != envelopeVersion {
		return nil, personalErrors.ErrInvalidEnvelope
	}
	nonceEnd := 1 + aead.NonceSize()
	plaintext, err := aead.Open(nil, data[1:nonceEnd], data[nonceEnd:], aad)
	if err != nil {
		return nil, personalErrors.ErrInvalidEnvelope
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, personalErrors.ErrInvalidEnvelope
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, personalErrors.ErrInvalidEnvelope
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, personalErrors.ErrInvalidEnvelope
	}
	return aead, nil
}
