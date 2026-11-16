package app_test

import (
	"context"
	"errors"
	"testing"

	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
)

// The legal check runs before anything touches a repository, so this test
// needs no database: nil dependencies would panic if the signup went on.
func TestSignupOwnerRequiresLegalAcceptance(t *testing.T) {
	uc := usersApp.NewSignupOwner(nil, nil, nil, nil, nil, 30)
	_, err := uc.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName:        "Dueña Prueba",
		Email:           "duena@example.test",
		Password:        "supersecret123",
		PasswordConfirm: "supersecret123",
	})
	if !errors.Is(err, userErrors.ErrLegalNotAccepted) {
		t.Fatalf("want ErrLegalNotAccepted, got %v", err)
	}
}
