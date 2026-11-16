package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/google/uuid"
)

func TestVaultSensitiveFormattingAndMalformedProofFailsBeforeIO(t *testing.T) {
	secret := strings.Repeat("a", 43)
	result := VaultResult{ID: uuid.NewString(), KeyVersion: "test", DataKey: secret}
	for _, value := range []any{result, personalVaultResponse{Vault: result}, UnlockPersonalVaultInput{ProofToken: secret}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if rendered := fmt.Sprintf(format, value); strings.Contains(rendered, secret) || !strings.Contains(rendered, "redacted") {
				t.Fatal("sensitive vault response rendered in diagnostics")
			}
		}
	}
	actor := security.AccessClaims{AccountID: uuid.New(), SessionID: uuid.New(), DeviceID: uuid.New()}
	u := &UnlockPersonalVault{}
	if _, err := u.Execute(context.Background(), actor, UnlockPersonalVaultInput{OperationID: uuid.New(), RequestHash: [32]byte{1}, VaultID: uuid.New(), ProofToken: strings.Repeat("*", 32)}); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("malformed proof did not fail with proof error before I/O")
	}
	rotation := &RotateRecoveryKey{}
	if _, err := rotation.Execute(context.Background(), actor, RotateRecoveryKeyInput{OperationID: uuid.New(), RequestHash: [32]byte{1}, KeyID: uuid.New(), ProofToken: strings.Repeat("*", 32)}); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("malformed recovery rotation proof classified as storage error")
	}
}
