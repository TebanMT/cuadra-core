package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRequirePermission_NoGrantsIsAnInternalCall(t *testing.T) {
	for _, p := range AllPermissions {
		if err := RequirePermission(context.Background(), p); err != nil {
			t.Errorf("%s without grants: %v", p, err)
		}
	}
}

func TestRequirePermission_OwnerAndOperator(t *testing.T) {
	owner := WithGrants(context.Background(), OwnerGrants())
	operator := WithGrants(context.Background(), OperatorGrants([]Permission{PermCredit}))
	for _, p := range AllPermissions {
		if err := RequirePermission(owner, p); err != nil {
			t.Errorf("owner %s: %v", p, err)
		}
		err := RequirePermission(operator, p)
		if p == PermCredit {
			if err != nil {
				t.Errorf("operator with credit: %v", err)
			}
			continue
		}
		if !errors.Is(err, ErrPermissionDenied) {
			t.Errorf("operator %s: err = %v, want denied", p, err)
		}
		var ce CustomError
		if !errors.As(err, &ce) || ce.ErrorCode != CodeForbidden || ce.Data["permission"] != string(p) {
			t.Errorf("operator %s: err = %#v, want CodeForbidden with the permission", p, err)
		}
	}
}

func TestPermissionCatalogIsComplete(t *testing.T) {
	seen := map[Permission]bool{}
	for _, p := range AllPermissions {
		if seen[p] {
			t.Errorf("duplicate %s", p)
		}
		seen[p] = true
		if !IsKnownPermission(p) {
			t.Errorf("%s has no message", p)
		}
		msg := PermissionDeniedMessage(p)
		if !strings.HasPrefix(msg, "No tienes permiso para ") || !strings.HasSuffix(msg, "Pídele al dueño que te lo active.") {
			t.Errorf("%s message = %q", p, msg)
		}
	}
	if len(AllPermissions) != len(permissionActions) {
		t.Errorf("catalog has %d permissions, messages %d", len(AllPermissions), len(permissionActions))
	}
	if IsKnownPermission("see_totals") {
		t.Error("unknown permission accepted")
	}
}
