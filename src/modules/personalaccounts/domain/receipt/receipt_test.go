package receipt_test

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	"github.com/google/uuid"
)

func TestReceiptExpiryDoesNotForgetEffect(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 123456789, time.UTC)
	hash := sha256.Sum256([]byte("request"))
	r, err := receipt.New(uuid.New(), receipt.Owner{AttemptID: uuid.New()}, "auth_start", hash, now)
	if err != nil {
		t.Fatal(err)
	}
	envelopes, err := crypto.NewEnvelopes("test", map[string][]byte{"test": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := envelopes.SealReceipt(r.Context(), []byte(`{"status":"pending"}`))
	if err != nil {
		t.Fatal(err)
	}
	expiry := r.CompletedAt.Add(10 * time.Minute)
	if err = r.SetResponse(sealed, expiry, r.Owner.AttemptID); err != nil {
		t.Fatal(err)
	}
	sealed.Ciphertext[0] ^= 1 // Constructor must not retain caller-owned buffers.
	e, err := r.ReadResponse(expiry.Add(-time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := envelopes.OpenReceipt(r.Context(), e)
	if err != nil || string(plain) != `{"status":"pending"}` {
		t.Fatal("receipt envelope mismatch")
	}
	if _, err = r.ReadResponse(expiry); !errors.Is(err, personalErrors.ErrResultExpired) {
		t.Fatal("boundary did not expire")
	}
	if err = r.Match("auth_start", hash); err != nil {
		t.Fatal("expiry forgot operation identity")
	}
	if err = r.Match("auth_start", sha256.Sum256([]byte("other"))); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("changed request accepted")
	}
	if err = r.SetResponse(e, expiry.Add(time.Millisecond), r.Owner.AttemptID); !errors.Is(err, personalErrors.ErrInvalidReceipt) {
		t.Fatal("response lifetime extended")
	}
}

func TestReceiptRejectsIncompleteIdentityAndUnpairedEnvelope(t *testing.T) {
	now := time.Now()
	if _, err := receipt.New(uuid.New(), receipt.Owner{}, "auth_start", sha256.Sum256([]byte("x")), now); err == nil {
		t.Fatal("owner required")
	}
	r, err := receipt.New(uuid.New(), receipt.Owner{AccountID: uuid.New()}, "recovery_key", sha256.Sum256([]byte("x")), now)
	if err != nil {
		t.Fatal(err)
	}
	r.Response.KeyVersion = "orphan"
	if err = r.Validate(); err == nil {
		t.Fatal("unpaired envelope accepted")
	}
}
