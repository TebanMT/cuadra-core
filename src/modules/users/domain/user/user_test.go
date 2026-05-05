package user_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
)

func TestValidateEmail(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"esteban@gym.com", true},
		{"a@b.co", true},
		{"no-at-sign.com", false},
		{"missing@domain", false},
		{"", false},
		{strings.Repeat("a", 256) + "@b.co", false},
	}
	for _, tc := range cases {
		if got := userDomain.ValidateEmail(tc.in); got != tc.want {
			t.Errorf("ValidateEmail(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := userDomain.ValidatePassword("12345678"); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
	if err := userDomain.ValidatePassword("short"); err == nil {
		t.Errorf("expected error for short password")
	}
}

func TestValidateFullName(t *testing.T) {
	if err := userDomain.ValidateFullName("Esteban Mares"); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
	if err := userDomain.ValidateFullName("ab"); err == nil {
		t.Errorf("expected error for too-short name")
	}
	if err := userDomain.ValidateFullName("foo@bar.com"); err != userErrors.ErrNameLooksLikeEmail {
		t.Errorf("email-shaped name: got %v, want ErrNameLooksLikeEmail", err)
	}
}

func TestSetActive(t *testing.T) {
	now := time.Now().UTC()
	owner := userDomain.NewUser(uuid.New(), uuid.New(), "owner@gym.com", "h", "Owner", userDomain.RoleOwner, false, nil, now)
	op := userDomain.NewUser(uuid.New(), owner.GymID, "op@gym.com", "h", "Op", userDomain.RoleOperator, false, nil, now)

	// owner deactivating themselves
	if err := owner.SetActive(false, owner.ID, now); err != userErrors.ErrSelfDeactivate {
		t.Errorf("self-deactivate: got %v, want ErrSelfDeactivate", err)
	}
	// caller deactivating an owner
	if err := owner.SetActive(false, op.ID, now); err != userErrors.ErrCannotDeactivateOwner {
		t.Errorf("deactivate-owner: got %v, want ErrCannotDeactivateOwner", err)
	}
	// owner deactivating an operator (caller != target, target not owner)
	if err := op.SetActive(false, owner.ID, now); err != nil {
		t.Errorf("operator deactivation: %v", err)
	}
	if op.Active {
		t.Errorf("operator should be inactive")
	}
}

func TestPromoteAndDemote(t *testing.T) {
	now := time.Now().UTC()
	op := userDomain.NewUser(uuid.New(), uuid.New(), "op@gym.com", "h", "Op", userDomain.RoleOperator, false, nil, now)
	op.PromoteToOwner(now)
	if !op.IsOwner() {
		t.Errorf("expected owner")
	}
	op.DemoteToOperator(now)
	if !op.IsOperator() {
		t.Errorf("expected operator")
	}
}
