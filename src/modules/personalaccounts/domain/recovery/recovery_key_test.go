package recovery

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/google/uuid"
)

func keyFixture(t *testing.T) *RecoveryKey {
	t.Helper()
	k, err := NewKey(uuid.New(), uuid.New(), 1, security.RecoveryVerifier{KeyVersion: "kit-v1", Digest: bytes.Repeat([]byte{0x41}, 32)}, time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRecoveryPreparationIsExplicitAndConsumeIsSingleUse(t *testing.T) {
	k := keyFixture(t)
	if !k.Live() || k.Prepared() || k.PreparedAt != nil {
		t.Fatal("issued kit was reported saved")
	}
	first := k.CreatedAt.Add(time.Second)
	if err := k.ConfirmSaved(first); err != nil || !k.Prepared() {
		t.Fatal(err)
	}
	if err := k.ConfirmSaved(first.Add(time.Second)); err != nil || !k.PreparedAt.Equal(first) {
		t.Fatal("confirmation was not idempotent")
	}
	if err := k.Consume(first.Add(2 * time.Second)); err != nil || k.Live() || k.Prepared() {
		t.Fatal("consumed key still live")
	}
	for _, transition := range []func() error{func() error { return k.Consume(first.Add(3 * time.Second)) }, func() error { return k.ConfirmSaved(first.Add(3 * time.Second)) }, func() error { return k.Revoke(first.Add(3 * time.Second)) }} {
		if err := transition(); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
			t.Fatal("consumed key reused")
		}
	}
}

func TestRecoveryRevocationAndUnconfirmedPossession(t *testing.T) {
	k := keyFixture(t)
	stamp := k.CreatedAt.Add(time.Second)
	if err := k.Revoke(stamp); err != nil || k.Live() {
		t.Fatal(err)
	}
	if err := k.Revoke(stamp.Add(time.Second)); err != nil || !k.RevokedAt.Equal(stamp) {
		t.Fatal("revocation overwritten")
	}
	if err := k.Consume(stamp.Add(time.Second)); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
		t.Fatal("revoked key consumed")
	}
	unconfirmed := keyFixture(t)
	if err := unconfirmed.Consume(unconfirmed.CreatedAt.Add(time.Second)); err != nil || unconfirmed.PreparedAt != nil {
		t.Fatal("valid possession forced a fabricated saved confirmation")
	}
}

func TestRecoveryVerifierIsCopiedValidatedAndRedacted(t *testing.T) {
	verifier := security.RecoveryVerifier{KeyVersion: "kit-v1", Digest: bytes.Repeat([]byte{0x41}, 32)}
	k, err := NewKey(uuid.New(), uuid.New(), 1, verifier, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	verifier.Digest[0] = 0
	if k.Verifier.Digest[0] != 0x41 {
		t.Fatal("verifier aliases caller")
	}
	for _, rendered := range []string{fmt.Sprintf("%+v", k), fmt.Sprintf("%#v", *k)} {
		if strings.Contains(rendered, "AAAA") || strings.Contains(rendered, "65 65") {
			t.Fatal("verifier leaked to diagnostics")
		}
	}
	for _, invalid := range []security.RecoveryVerifier{{}, {KeyVersion: "kit-v1", Digest: make([]byte, 32)}, {KeyVersion: " bad ", Digest: bytes.Repeat([]byte{1}, 32)}, {KeyVersion: "kit-v1", Digest: []byte{1}}} {
		if _, err := NewKey(uuid.New(), uuid.New(), 1, invalid, time.Now()); !errors.Is(err, personalErrors.ErrInvalidRecoveryKey) {
			t.Fatal("invalid verifier accepted")
		}
	}
}
