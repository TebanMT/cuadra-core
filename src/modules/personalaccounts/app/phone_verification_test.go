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

func TestPhoneCommandRejectsInvalidInputBeforeDependencies(t *testing.T) {
	valid := phoneOperation{OperationID: uuid.New(), Binding: attempt.Binding{ID: uuid.New(), ContinuationHash: [32]byte{1}}, RequestHash: [32]byte{2}, Origin: "127.0.0.1"}
	for name, change := range map[string]func(*phoneOperation){"operation": func(in *phoneOperation) { in.OperationID = uuid.Nil }, "attempt": func(in *phoneOperation) { in.Binding.ID = uuid.Nil }, "credential": func(in *phoneOperation) { in.Binding.ContinuationHash = [32]byte{} }, "hash": func(in *phoneOperation) { in.RequestHash = [32]byte{} }, "origin": func(in *phoneOperation) { in.Origin = "  " }} {
		t.Run(name, func(t *testing.T) {
			in := valid
			change(&in)
			if !errors.Is(in.validate(), personalErrors.ErrInvalidAuthAttempt) {
				t.Fatal("invalid command accepted")
			}
		})
	}
	u := &CheckPhoneVerification{}
	for _, code := range []string{"", "123", "12345678901", "１２３４", "12x4", "12\n4"} {
		_, err := u.Execute(context.Background(), CheckPhoneVerificationInput{OperationID: valid.OperationID, Binding: valid.Binding, RequestHash: valid.RequestHash, Origin: valid.Origin, Code: code})
		if !errors.Is(err, personalErrors.ErrVerificationInput) {
			t.Fatal("invalid code reached dependencies", err)
		}
	}
	accountContext := shared.WithPersonalAccount(context.Background(), uuid.New())
	if _, err := u.Execute(accountContext, CheckPhoneVerificationInput{OperationID: valid.OperationID, Binding: valid.Binding, RequestHash: valid.RequestHash, Origin: valid.Origin, Code: "123456"}); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("prelogin inherited account mutex", err)
	}
	if _, err := (&ResendAuth{}).Execute(accountContext, ResendAuthInput{OperationID: valid.OperationID, Binding: valid.Binding, RequestHash: valid.RequestHash, Origin: valid.Origin, Channel: verification.SMS}); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal(err)
	}
	if _, err := (&GetAttempt{}).Execute(accountContext, valid.Binding); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal(err)
	}
	if _, err := (&StartAuth{}).Execute(accountContext, StartAuthInput{OperationID: valid.OperationID, Binding: valid.Binding, RequestHash: valid.RequestHash, Origin: valid.Origin, Purpose: attempt.Access}); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("start inherited account mutex", err)
	}
	if _, err := NewCheckPhoneVerification(StartAuthDependencies{}, 1); !errors.Is(err, personalErrors.ErrVerificationConfiguration) {
		t.Fatal(err)
	}
	if _, err := NewResendAuth(StartAuthDependencies{}, PhoneRetryPolicy{}); !errors.Is(err, personalErrors.ErrVerificationConfiguration) {
		t.Fatal(err)
	}
	if _, err := NewGetAttempt(GetAttemptDependencies{}); !errors.Is(err, personalErrors.ErrVerificationConfiguration) {
		t.Fatal(err)
	}
}

func TestPhoneTokenCapturesExactOperationRevision(t *testing.T) {
	a := &attempt.AuthAttempt{Revision: 7, ProviderOperation: &attempt.ProviderOperation{ID: uuid.New(), RequestHash: [32]byte{1}, Kind: attempt.Check}}
	token := phoneToken(a)
	a.Revision++
	a.ProviderOperation.RequestHash = [32]byte{2}
	if token.Revision != 7 || token.RequestHash != [32]byte{1} || token.Kind != attempt.Check {
		t.Fatal("operation token aliased attempt")
	}
	if phoneToken(&attempt.AuthAttempt{}) != (attempt.OperationToken{}) {
		t.Fatal("unreserved attempt produced operation")
	}
}
