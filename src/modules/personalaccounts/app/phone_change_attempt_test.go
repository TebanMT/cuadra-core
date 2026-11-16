package app

import (
	"context"
	"errors"
	"testing"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func TestPhoneChangeAttemptRejectsInheritedAuthorityBeforeDependencies(t *testing.T) {
	b := attempt.Binding{ID: uuid.New(), ContinuationHash: [32]byte{1}}
	in := ResendAuthInput{OperationID: uuid.New(), Binding: b, RequestHash: [32]byte{2}, Channel: verification.SMS, Origin: "127.0.0.1"}
	for _, ctx := range []context.Context{nil, shared.WithPersonalAccount(context.Background(), uuid.New()), shared.WithPersonalAttempt(context.Background(), b.ID, b.ContinuationHash), shared.WithTenant(context.Background(), uuid.New()), shared.WithPersonalAccount(context.Background(), uuid.Nil)} {
		if _, err := (&GetPhoneChangeAttempt{}).Execute(ctx, b); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("GET scope reached dependencies", err)
		}
		if _, err := (&ResendPhoneChange{}).Execute(ctx, in); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("resend scope reached dependencies", err)
		}
		if _, err := (&GetAuthStatusRouter{}).Execute(ctx, b); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("status router scope reached dependencies", err)
		}
		if _, err := (&ResendAuthRouter{}).Execute(ctx, in); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("resend router scope reached dependencies", err)
		}
	}
	if _, err := NewGetAuthStatusRouter(nil, nil, nil, nil); !errors.Is(err, personalErrors.ErrInvalidSecurityConfiguration) {
		t.Fatal(err)
	}
	if _, err := NewResendAuthRouter(nil, nil, nil, nil); !errors.Is(err, personalErrors.ErrInvalidSecurityConfiguration) {
		t.Fatal(err)
	}
}
