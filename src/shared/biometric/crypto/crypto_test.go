package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/google/uuid"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	gmk := make([]byte, GMKSize)
	if _, err := rand.Read(gmk); err != nil {
		t.Fatalf("seed gmk: %v", err)
	}
	plaintext := []byte("template-bytes-of-arbitrary-length-0123456789ABCDEF")

	blob, err := EncryptTemplate(gmk, plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if blob[0] != CurrentVersion {
		t.Errorf("expected version byte %#x, got %#x", CurrentVersion, blob[0])
	}
	// nonce(12) + version(1) + at least tag(16) + len(plaintext).
	if len(blob) < 1+12+16+len(plaintext) {
		t.Errorf("blob too short: %d bytes", len(blob))
	}

	got, err := DecryptTemplate(gmk, blob)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round-trip mismatch:\n got:  %q\n want: %q", got, plaintext)
	}
}

func TestEncryptRejectsBadGMK(t *testing.T) {
	if _, err := EncryptTemplate([]byte("too-short"), []byte("data")); err != ErrInvalidGMK {
		t.Errorf("expected ErrInvalidGMK, got %v", err)
	}
	if _, err := DecryptTemplate(make([]byte, 8), []byte{1, 2, 3}); err != ErrInvalidGMK {
		t.Errorf("expected ErrInvalidGMK, got %v", err)
	}
}

func TestDecryptRejectsTamperedBlob(t *testing.T) {
	gmk := make([]byte, GMKSize)
	_, _ = rand.Read(gmk)
	blob, _ := EncryptTemplate(gmk, []byte("plaintext"))

	// Flip a byte in the ciphertext region — GCM tag verification must fail.
	tampered := append([]byte{}, blob...)
	tampered[len(tampered)-3] ^= 0xFF
	if _, err := DecryptTemplate(gmk, tampered); err != ErrDecryptionFailed {
		t.Errorf("expected ErrDecryptionFailed on tampered blob, got %v", err)
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	gmk1 := make([]byte, GMKSize)
	gmk2 := make([]byte, GMKSize)
	_, _ = rand.Read(gmk1)
	_, _ = rand.Read(gmk2)
	blob, _ := EncryptTemplate(gmk1, []byte("plaintext"))
	if _, err := DecryptTemplate(gmk2, blob); err != ErrDecryptionFailed {
		t.Errorf("expected ErrDecryptionFailed with wrong key, got %v", err)
	}
}

func TestDecryptRejectsUnsupportedVersion(t *testing.T) {
	gmk := make([]byte, GMKSize)
	_, _ = rand.Read(gmk)
	blob, _ := EncryptTemplate(gmk, []byte("plaintext"))
	blob[0] = 0xFF
	if _, err := DecryptTemplate(gmk, blob); err != ErrUnsupportedVersion {
		t.Errorf("expected ErrUnsupportedVersion, got %v", err)
	}
}

func TestDecryptRejectsShortBlob(t *testing.T) {
	gmk := make([]byte, GMKSize)
	_, _ = rand.Read(gmk)
	if _, err := DecryptTemplate(gmk, []byte{}); err != ErrBlobTooShort {
		t.Errorf("expected ErrBlobTooShort on empty, got %v", err)
	}
	if _, err := DecryptTemplate(gmk, []byte{CurrentVersion, 0, 0}); err != ErrBlobTooShort {
		t.Errorf("expected ErrBlobTooShort on truncated, got %v", err)
	}
}

func TestNonceUniqueness(t *testing.T) {
	// Two encryptions of the same plaintext under the same key must yield
	// different blobs (proves the nonce is fresh per call).
	gmk := make([]byte, GMKSize)
	_, _ = rand.Read(gmk)
	a, _ := EncryptTemplate(gmk, []byte("same-plaintext"))
	b, _ := EncryptTemplate(gmk, []byte("same-plaintext"))
	if bytes.Equal(a, b) {
		t.Errorf("expected distinct ciphertexts (nonce must be random per call)")
	}
}

func TestInMemoryGMKProvider(t *testing.T) {
	p := NewInMemoryGMKProvider()
	gym := uuid.New()

	if _, err := p.GetGMK(nil, gym); err != ErrGMKNotFound {
		t.Errorf("expected ErrGMKNotFound for unknown gym, got %v", err)
	}

	p.SetDeterministic(gym, "test-seed")
	k1, err := p.GetGMK(nil, gym)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(k1) != GMKSize {
		t.Errorf("expected %d-byte GMK, got %d", GMKSize, len(k1))
	}

	// SetDeterministic must be idempotent for the same (gym, seed).
	p2 := NewInMemoryGMKProvider()
	p2.SetDeterministic(gym, "test-seed")
	k2, _ := p2.GetGMK(nil, gym)
	if !bytes.Equal(k1, k2) {
		t.Errorf("deterministic seeding mismatch across providers")
	}

	// Forget evicts.
	p.Forget(gym)
	if _, err := p.GetGMK(nil, gym); err != ErrGMKNotFound {
		t.Errorf("expected ErrGMKNotFound after Forget, got %v", err)
	}

	// Set rejects bad sizes.
	if err := p.Set(gym, []byte("short")); err != ErrInvalidGMK {
		t.Errorf("expected ErrInvalidGMK from Set, got %v", err)
	}
}
