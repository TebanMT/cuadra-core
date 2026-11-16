package reports

import (
	"context"
	"errors"
	"testing"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Withdrawing needs PermCashOut; the check runs before any read, so a bare
// use case is enough here.
func TestCashClose_WithdrawNeedsCashOut(t *testing.T) {
	operator := sharedDomain.WithGrants(context.Background(), sharedDomain.OperatorGrants(nil))
	uc := &CashClose{}
	if _, err := uc.Close(operator, CashCloseInput{Withdraw: true}); !errors.Is(err, sharedDomain.ErrPermissionDenied) {
		t.Errorf("close with withdraw: err = %v, want denied", err)
	}
	if _, err := uc.Withdraw(operator, CashWithdrawInput{}); !errors.Is(err, sharedDomain.ErrPermissionDenied) {
		t.Errorf("withdraw: err = %v, want denied", err)
	}
}
