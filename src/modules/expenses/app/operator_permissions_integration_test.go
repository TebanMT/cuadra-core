//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"testing"

	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	repos "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func TestOperatorPermissions_CashOutflow(t *testing.T) {
	f := setupExpenses(t)
	uc := expApp.NewCreateCashMovement(repos.NewCashMovementSQLiteRepository(), f.uow, f.recorder, expApp.OperationalPolicy{})
	operator := sharedDomain.WithGrants(context.Background(), sharedDomain.OperatorGrants(nil))
	day := mexicoBusinessDay(t)

	_, err := uc.Execute(operator, expApp.CashMovementInput{GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day,
		Amount: 50, MovementType: cashDomain.CashOut, Reason: "Garrafón", IdempotencyKey: "out-1"})
	if !errors.Is(err, sharedDomain.ErrPermissionDenied) {
		t.Fatalf("cash out without permission: err = %v", err)
	}
	if _, err := uc.Execute(operator, expApp.CashMovementInput{GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day,
		Amount: 50, MovementType: cashDomain.CashIn, Reason: "Cambio", IdempotencyKey: "in-1"}); err != nil {
		t.Fatalf("adding cash stays open: %v", err)
	}
	withCashOut := sharedDomain.WithGrants(context.Background(),
		sharedDomain.OperatorGrants([]sharedDomain.Permission{sharedDomain.PermCashOut}))
	if _, err := uc.Execute(withCashOut, expApp.CashMovementInput{GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day,
		Amount: 50, MovementType: cashDomain.CashOut, Reason: "Garrafón", IdempotencyKey: "out-2"}); err != nil {
		t.Fatalf("operator with cash_out: %v", err)
	}
}
