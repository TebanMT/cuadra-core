package user_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func newTestOperator(t *testing.T) *userDomain.User {
	t.Helper()
	u, err := userDomain.NewOperator(uuid.New(), uuid.New(), "Ana Recepción", "+524421234567", nil, "", uuid.New(), time.Now().UTC())
	if err != nil {
		t.Fatalf("NewOperator: %v", err)
	}
	return u
}

func TestNewOperatorStartsWithoutPermissions(t *testing.T) {
	u := newTestOperator(t)
	if len(u.Permissions) != 0 || len(u.EffectivePermissions()) != 0 {
		t.Fatalf("new operator permissions = %v", u.Permissions)
	}
	for _, p := range sharedDomain.AllPermissions {
		if u.Can(p) {
			t.Errorf("new operator can %s", p)
		}
	}
}

func TestSetPermissions_CleansAndOrders(t *testing.T) {
	u := newTestOperator(t)
	v := u.Version
	err := u.SetPermissions([]sharedDomain.Permission{
		sharedDomain.PermInventory, sharedDomain.PermCredit, sharedDomain.PermInventory,
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	want := []sharedDomain.Permission{sharedDomain.PermCredit, sharedDomain.PermInventory}
	if !reflect.DeepEqual(u.Permissions, want) || !reflect.DeepEqual(u.EffectivePermissions(), want) {
		t.Errorf("permissions = %v, want %v", u.Permissions, want)
	}
	if u.Version != v+1 {
		t.Errorf("version = %d, want %d", u.Version, v+1)
	}
	if !u.Can(sharedDomain.PermCredit) || u.Can(sharedDomain.PermCashOut) {
		t.Error("Can does not follow the stored permissions")
	}

	if err := u.SetPermissions([]sharedDomain.Permission{"see_totals"}, time.Now().UTC()); !errors.Is(err, userErrors.ErrUnknownPermission) {
		t.Errorf("unknown permission err = %v", err)
	}
	if !reflect.DeepEqual(u.Permissions, want) {
		t.Errorf("a rejected change modified the permissions: %v", u.Permissions)
	}
	if err := u.SetPermissions(nil, time.Now().UTC()); err != nil || len(u.Permissions) != 0 {
		t.Errorf("clearing: err=%v permissions=%v", err, u.Permissions)
	}
}

func TestOwnerAlwaysHasEveryPermission(t *testing.T) {
	owner, err := userDomain.NewOwner(uuid.New(), uuid.New(), "owner@gym.com", "hash", "Dueño", false, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(owner.EffectivePermissions(), sharedDomain.AllPermissions) {
		t.Errorf("owner permissions = %v", owner.EffectivePermissions())
	}
	if err := owner.SetPermissions(nil, time.Now().UTC()); !errors.Is(err, userErrors.ErrOwnerHasAllPermissions) {
		t.Errorf("owner SetPermissions err = %v", err)
	}
}
