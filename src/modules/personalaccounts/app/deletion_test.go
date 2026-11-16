package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func TestDeletionRejectsScopesAndMissingPolicyBeforeDependencies(t *testing.T) {
	actor := security.AccessClaims{AccountID: uuid.New(), SessionID: uuid.New(), DeviceID: uuid.New()}
	u := &RequestAccountDeletion{}
	if _, err := u.Execute(context.Background(), actor, RequestAccountDeletionInput{}); !errors.Is(err, personalErrors.ErrDeletionPolicyNotConfigured) {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{nil, shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithTenant(context.Background(), uuid.New()), shared.WithPersonalAccount(context.Background(), uuid.New())} {
		if _, err := u.Execute(ctx, actor, RequestAccountDeletionInput{}); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("inherited authority accepted", err)
		}
	}
	get := &GetDeletionResult{}
	for _, ctx := range []context.Context{nil, shared.WithPersonalAccount(context.Background(), actor.AccountID), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err := get.Execute(ctx, GetDeletionResultInput{}); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("limited read accepted authority", err)
		}
	}
	if _, err := NewRequestAccountDeletion(RequestAccountDeletionDependencies{}); err == nil {
		t.Fatal("missing ports accepted")
	}
	if _, err := NewGetDeletionResult(GetDeletionResultDependencies{}); err == nil {
		t.Fatal("missing ports accepted")
	}
	secret := "sensitive-test-credential"
	if strings.Contains(fmt.Sprintf("%#v", RequestAccountDeletionInput{ResultSecret: secret, ProofToken: secret}), secret) || strings.Contains(fmt.Sprintf("%#v", GetDeletionResultInput{ResultSecret: secret}), secret) {
		t.Fatal("formatter disclosed secret")
	}
}
