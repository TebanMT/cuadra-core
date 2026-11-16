package security

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"testing"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	personalSecurity "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
)

func testEnvelopes(t *testing.T) *Envelopes {
	t.Helper()
	s, err := NewEnvelopes("wrap-v1", map[string][]byte{"wrap-v1": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVaultEnvelopeRecoversKeyAndUsesS00AAD(t *testing.T) {
	s := testEnvelopes(t)
	context := personalSecurity.VaultContext{AccountID: accountID, VaultID: proofID}
	dataKey := bytes.Repeat([]byte{9}, 32)
	wrapped, err := s.WrapVault(context, dataKey)
	if err != nil {
		t.Fatal(err)
	}
	// A separate AES-GCM reader checks the envelope and the literal S00 AAD,
	// rather than relying solely on our own round-trip implementation.
	block, _ := aes.NewCipher(bytes.Repeat([]byte{7}, 32))
	gcm, _ := cipher.NewGCM(block)
	aad := []byte(`{"account_id":"11111111-1111-4111-8111-111111111111","key_version":"wrap-v1","vault_id":"44444444-4444-4444-8444-444444444444"}`)
	plain, err := gcm.Open(nil, wrapped.Ciphertext[1:13], wrapped.Ciphertext[13:], aad)
	if err != nil || !bytes.Equal(plain, dataKey) {
		t.Fatal("S00 AAD mismatch", err)
	}
	clear(dataKey)
	recovered, err := s.UnwrapVault(context, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recovered, bytes.Repeat([]byte{9}, 32)) {
		t.Fatal("key was not recoverable after losing its local copy")
	}
	second, err := s.WrapVault(context, recovered)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(wrapped.Ciphertext[1:13], second.Ciphertext[1:13]) {
		t.Fatal("AES-GCM nonce reused")
	}
}

func TestEnvelopesRejectCrossScopeAndTampering(t *testing.T) {
	s := testEnvelopes(t)
	context := personalSecurity.VaultContext{AccountID: accountID, VaultID: proofID}
	wrapped, err := s.WrapVault(context, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []personalSecurity.VaultContext{{AccountID: operationID, VaultID: proofID}, {AccountID: accountID, VaultID: operationID}} {
		if _, err := s.UnwrapVault(wrong, wrapped); !errors.Is(err, personalErrors.ErrInvalidEnvelope) {
			t.Fatal("cross-scope key accepted", err)
		}
	}
	for _, offset := range []int{0, 1, 12, 13, len(wrapped.Ciphertext) - 1} {
		corrupt := wrapped
		corrupt.Ciphertext = bytes.Clone(wrapped.Ciphertext)
		corrupt.Ciphertext[offset] ^= 1
		if _, err := s.UnwrapVault(context, corrupt); !errors.Is(err, personalErrors.ErrInvalidEnvelope) {
			t.Fatal("tampering accepted", offset, err)
		}
	}
	for _, length := range []int{0, 1, 12, 13, 28} {
		corrupt := wrapped
		corrupt.Ciphertext = wrapped.Ciphertext[:length]
		if _, err := s.UnwrapVault(context, corrupt); !errors.Is(err, personalErrors.ErrInvalidEnvelope) {
			t.Fatal("truncated envelope accepted", length, err)
		}
	}
	for _, version := range []string{"", "unknown", "wrap-v2"} {
		corrupt := wrapped
		corrupt.KeyVersion = version
		if _, err := s.UnwrapVault(context, corrupt); !errors.Is(err, personalErrors.ErrInvalidEnvelope) {
			t.Fatal("wrong key version accepted", err)
		}
	}
	wrong, err := NewEnvelopes("wrap-v1", map[string][]byte{"wrap-v1": bytes.Repeat([]byte{8}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.UnwrapVault(context, wrapped); !errors.Is(err, personalErrors.ErrInvalidEnvelope) {
		t.Fatal("wrong key accepted", err)
	}
	// Even the same key under a different version cannot authenticate old AAD.
	s.keys["alias"] = bytes.Repeat([]byte{7}, 32)
	wrapped.KeyVersion = "alias"
	if _, err := s.UnwrapVault(context, wrapped); !errors.Is(err, personalErrors.ErrInvalidEnvelope) {
		t.Fatal("key version not authenticated", err)
	}
}

func TestReceiptBindsAccountAttemptOperationPurposeAndVersion(t *testing.T) {
	s := testEnvelopes(t)
	context := personalSecurity.ReceiptContext{AccountID: accountID, AttemptID: proofID, OperationID: operationID, Purpose: "recovery"}
	plaintext := []byte(`{"synthetic_response":"test"}`)
	sealed, err := s.SealReceipt(context, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := s.OpenReceipt(context, sealed)
	if err != nil || !bytes.Equal(recovered, plaintext) {
		t.Fatal("receipt round trip failed", err)
	}
	for _, mutate := range []func(*personalSecurity.ReceiptContext){
		func(c *personalSecurity.ReceiptContext) { c.AccountID = sessionID }, func(c *personalSecurity.ReceiptContext) { c.AttemptID = sessionID },
		func(c *personalSecurity.ReceiptContext) { c.OperationID = sessionID }, func(c *personalSecurity.ReceiptContext) { c.Purpose = "refresh" },
	} {
		changed := context
		mutate(&changed)
		if _, err := s.OpenReceipt(changed, sealed); !errors.Is(err, personalErrors.ErrInvalidEnvelope) {
			t.Fatal("receipt crossed context", err)
		}
	}
	if _, err := s.UnwrapVault(personalSecurity.VaultContext{AccountID: accountID, VaultID: operationID}, sealed); !errors.Is(err, personalErrors.ErrInvalidEnvelope) {
		t.Fatal("receipt reused as vault key", err)
	}
	for _, context := range []personalSecurity.ReceiptContext{
		{AttemptID: proofID, OperationID: operationID, Purpose: "signup"},
		{AccountID: accountID, OperationID: operationID, Purpose: "refresh"},
	} {
		sealed, err := s.SealReceipt(context, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.OpenReceipt(context, sealed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SealReceipt(personalSecurity.ReceiptContext{OperationID: operationID, Purpose: "signup"}, plaintext); !errors.Is(err, personalErrors.ErrInvalidEnvelope) {
		t.Fatal("unowned receipt accepted", err)
	}
}

func TestSymmetricKeyRotationCopiesAndValidatesConfiguration(t *testing.T) {
	old := testEnvelopes(t)
	context := personalSecurity.VaultContext{AccountID: accountID, VaultID: proofID}
	wrapped, err := old.WrapVault(context, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{8}, 32)
	keys := map[string][]byte{"wrap-v1": bytes.Repeat([]byte{7}, 32), "wrap-v2": key}
	rotated, err := NewEnvelopes("wrap-v2", keys)
	if err != nil {
		t.Fatal(err)
	}
	key[0] ^= 1
	delete(keys, "wrap-v1")
	if _, err := rotated.UnwrapVault(context, wrapped); err != nil {
		t.Fatal("previous version missing", err)
	}
	newWrapped, err := rotated.WrapVault(context, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if newWrapped.KeyVersion != "wrap-v2" {
		t.Fatal("not using active version")
	}
	for _, keys := range []map[string][]byte{nil, {"v1": make([]byte, 16)}, {"v2": make([]byte, 32)}, {"v1": make([]byte, 32), "": make([]byte, 32)}} {
		if _, err := NewEnvelopes("v1", keys); !errors.Is(err, personalErrors.ErrInvalidSecurityConfiguration) {
			t.Fatal("invalid encryption configuration accepted", err)
		}
		if _, err := NewRecoveryKeys("v1", keys); !errors.Is(err, personalErrors.ErrInvalidSecurityConfiguration) {
			t.Fatal("invalid HMAC configuration accepted", err)
		}
	}
}

func TestRecoveryHMACGoldenRotationAndConstantComparison(t *testing.T) {
	keys := map[string][]byte{"kit-v1": bytes.Repeat([]byte{5}, 32)}
	s, err := NewRecoveryKeys("kit-v1", keys)
	if err != nil {
		t.Fatal(err)
	}
	secret := rawURL.EncodeToString(sampleDeviceProof().Nonce)
	verifier, err := s.CreateVerifier(secret)
	if err != nil {
		t.Fatal(err)
	}
	// Independent Python hmac.new(key, secret.encode(), hashlib.sha256) vector.
	if hex.EncodeToString(verifier.Digest) != "b9b8a57e1af88d6312902dceb27fbe67d2877c413877a28ac50e2b8b595d31a9" {
		t.Fatal("HMAC vector mismatch")
	}
	if verifier.KeyVersion != "kit-v1" {
		t.Fatal("verifier lost its key version")
	}
	if err := s.Verify(secret, verifier); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{"", secret + "=", secret[:42], rawURL.EncodeToString(bytes.Repeat([]byte{9}, 32))} {
		if err := s.Verify(candidate, verifier); !errors.Is(err, personalErrors.ErrInvalidRecoveryKey) {
			t.Fatal("wrong kit accepted", err)
		}
	}
	for _, offset := range []int{0, 15, 31} {
		corrupt := verifier
		corrupt.Digest = bytes.Clone(verifier.Digest)
		corrupt.Digest[offset] ^= 1
		if err := s.Verify(secret, corrupt); !errors.Is(err, personalErrors.ErrInvalidRecoveryKey) {
			t.Fatal("wrong verifier accepted", err)
		}
	}
	keys["kit-v2"] = bytes.Repeat([]byte{6}, 32)
	rotated, err := NewRecoveryKeys("kit-v2", keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := rotated.Verify(secret, verifier); err != nil {
		t.Fatal("old kit invalid during rotation", err)
	}
	current, err := rotated.CreateVerifier(secret)
	if err != nil {
		t.Fatal(err)
	}
	if current.KeyVersion != "kit-v2" || bytes.Equal(current.Digest, verifier.Digest) {
		t.Fatal("new verifier did not rotate")
	}
	for _, version := range []string{"unknown", "kit-v2"} {
		wrong := verifier
		wrong.KeyVersion = version
		if err := rotated.Verify(secret, wrong); !errors.Is(err, personalErrors.ErrInvalidRecoveryKey) {
			t.Fatal("wrong version accepted", err)
		}
	}
	delete(keys, "kit-v1")
	if err := rotated.Verify(secret, verifier); err != nil {
		t.Fatal("key configuration not copied", err)
	}
	one, err := NewRecoverySecret()
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewRecoverySecret()
	if err != nil {
		t.Fatal(err)
	}
	if !validRecoverySecret(one) || !validRecoverySecret(two) || one == two {
		t.Fatal("kit secrets must contain fresh 256-bit entropy")
	}
}
