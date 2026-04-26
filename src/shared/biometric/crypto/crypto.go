// Package crypto implements ADR-006 §2 — AES-256-GCM encryption of biometric
// templates with the per-gym GMK (Gym Master Key).
//
// Wire format (ADR-006 §2.1):
//
//	[1 byte: version=0x01] [12 bytes: nonce] [N bytes: ciphertext+tag]
//
// We use the standard 12-byte GCM nonce (96 bits) — AES-GCM is specified for
// this size and the Go stdlib aead.NonceSize() returns 12. The tag (16 bytes)
// is appended to the ciphertext by Seal as part of the AEAD output, so we
// don't store it separately on the wire.
//
// The version byte exists so we can rotate to AES-256-SIV or post-quantum
// later without re-keying every row in one shot.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const (
	// CurrentVersion is the byte stamped on new blobs. DecryptTemplate refuses
	// any other value — bump it (and add a branch in DecryptTemplate) when
	// rotating to a new algorithm.
	CurrentVersion byte = 0x01

	// GMKSize is the byte length of a Gym Master Key. AES-256 ⇒ 32 bytes.
	GMKSize = 32
)

var (
	ErrInvalidGMK         = errors.New("gmk must be 32 bytes (256 bits)")
	ErrBlobTooShort       = errors.New("encrypted blob is too short to be valid")
	ErrUnsupportedVersion = errors.New("encrypted blob uses an unsupported version byte")
	ErrDecryptionFailed   = errors.New("template decryption failed (wrong key or corrupted blob)")
)

// EncryptTemplate encrypts plaintext under gmk with a freshly-generated nonce
// and returns the wire blob `[version][nonce][ciphertext+tag]`.
//
// Callers MUST NOT reuse the result for a different plaintext (the nonce is
// random per call — that's the GCM safety contract).
func EncryptTemplate(gmk, plaintext []byte) ([]byte, error) {
	if len(gmk) != GMKSize {
		return nil, ErrInvalidGMK
	}
	block, err := aes.NewCipher(gmk)
	if err != nil {
		return nil, fmt.Errorf("aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cipher.NewGCM: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("rand.Read: %w", err)
	}
	// Pre-allocate: 1 (version) + nonceSize + len(plaintext) + tagSize.
	out := make([]byte, 0, 1+aead.NonceSize()+len(plaintext)+aead.Overhead())
	out = append(out, CurrentVersion)
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// DecryptTemplate is the inverse of EncryptTemplate. Returns the plaintext
// template bytes; the caller is responsible for zeroing them after use.
func DecryptTemplate(gmk, blob []byte) ([]byte, error) {
	if len(gmk) != GMKSize {
		return nil, ErrInvalidGMK
	}
	if len(blob) < 1 {
		return nil, ErrBlobTooShort
	}
	if blob[0] != CurrentVersion {
		return nil, ErrUnsupportedVersion
	}
	block, err := aes.NewCipher(gmk)
	if err != nil {
		return nil, fmt.Errorf("aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cipher.NewGCM: %w", err)
	}
	nonceSize := aead.NonceSize()
	// 1 (version) + nonceSize + at least tagSize ciphertext.
	if len(blob) < 1+nonceSize+aead.Overhead() {
		return nil, ErrBlobTooShort
	}
	nonce := blob[1 : 1+nonceSize]
	ciphertext := blob[1+nonceSize:]
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	return plaintext, nil
}

// Zero overwrites b with zeros. Use it on plaintext templates as soon as
// you're done with them — defence in depth against memory snapshotting.
// Note: Go's GC may keep copies; this is best-effort, not a guarantee.
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
