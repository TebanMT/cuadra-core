package vault

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

func TestVaultOwnsOnlyBoundedEnvelopeAndRejectsInvalidState(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	envelope := security.Envelope{KeyVersion: "synthetic-v1", Ciphertext: bytes.Repeat([]byte{97}, 61)}
	v, err := New(uuid.New(), uuid.New(), envelope, now)
	if err != nil || !v.Live() || v.Context().AccountID != v.AccountID || v.Context().VaultID != v.ID {
		t.Fatal("valid vault rejected")
	}
	envelope.Ciphertext[0] = 1
	if v.Envelope.Ciphertext[0] != 97 {
		t.Fatal("constructor retained caller buffer")
	}
	for _, mutate := range []func(*Vault){
		func(v *Vault) { v.ID = uuid.Nil },
		func(v *Vault) { v.AccountID = uuid.Nil },
		func(v *Vault) { v.Envelope.KeyVersion = strings.Repeat("x", 81) },
		func(v *Vault) { v.Envelope.KeyVersion = "invalid\x00version" },
		func(v *Vault) { v.Envelope.Ciphertext = make([]byte, 28) },
		func(v *Vault) { v.Envelope.Ciphertext = make([]byte, 4097) },
		func(v *Vault) { v.CreatedAt = time.Time{} },
		func(v *Vault) { earlier := now.Add(-time.Second); v.RetiredAt = &earlier },
	} {
		copy := *v
		mutate(&copy)
		if !errors.Is(copy.Validate(), personalErrors.ErrVaultState) || copy.Live() {
			t.Fatal("invalid vault accepted")
		}
	}
	retired := now.Add(time.Second)
	v.RetiredAt = &retired
	if v.Validate() != nil || v.Live() {
		t.Fatal("retired vault became live")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if rendered := fmt.Sprintf(format, *v); !strings.Contains(rendered, "redacted") || strings.Contains(rendered, "synthetic-v1") {
			t.Fatal("vault formatting disclosed persisted envelope")
		}
	}
}
