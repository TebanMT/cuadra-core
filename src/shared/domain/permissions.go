package domain

import (
	"context"
	"errors"
)

// Permission is one action the owner can allow or deny to each operator
// (reception employee). The owner always has every permission. Work that is
// not listed here (check-ins, charging the full price, cash close without a
// withdrawal, member registration) is open to every operator.
//
// Checks happen in two places: route-level permissions are enforced by
// middleware.EnforceOperatorPermissions; permissions that depend on the
// request content (a discount, a partial payment, a cash outflow) are checked
// by the use case with RequirePermission.
type Permission string

const (
	// PermDiscounts: give a discount, change the enrollment or maintenance
	// amount, or record a charge with an earlier date.
	PermDiscounts Permission = "discounts"
	// PermCredit: charge a membership partially or sell on credit (fiado).
	PermCredit Permission = "credit"
	// PermCashOut: take cash out of a drawer (withdrawals and outflows).
	PermCashOut Permission = "cash_out"
	// PermViewTotals: see money totals (total charged, by method, the day's
	// cash) and payments or cash closes of days other than today.
	PermViewTotals Permission = "view_totals"
	// PermCheckinOverride: let a member with an expired membership or a debt in.
	PermCheckinOverride Permission = "checkin_override"
	// PermProducts: create, edit (price included), deactivate or reactivate products.
	PermProducts Permission = "products"
	// PermInventory: stock adjustments and stock counts.
	PermInventory Permission = "inventory"
	// PermMemberStatus: deactivate a member or mark them as lost.
	PermMemberStatus Permission = "member_status"
)

// AllPermissions is the catalog in display order.
var AllPermissions = []Permission{
	PermDiscounts, PermCredit, PermCashOut, PermViewTotals,
	PermCheckinOverride, PermProducts, PermInventory, PermMemberStatus,
}

// permissionActions completes "No tienes permiso para …".
var permissionActions = map[Permission]string{
	PermDiscounts:       "dar descuentos ni cambiar montos o fechas de cobro",
	PermCredit:          "fiar",
	PermCashOut:         "sacar dinero de la caja",
	PermViewTotals:      "ver totales de dinero ni cobros de otros días",
	PermCheckinOverride: "dejar pasar a un socio vencido",
	PermProducts:        "crear ni editar productos y precios",
	PermInventory:       "ajustar el inventario",
	PermMemberStatus:    "dar de baja socios",
}

// IsKnownPermission reports whether p is in the catalog.
func IsKnownPermission(p Permission) bool {
	_, ok := permissionActions[p]
	return ok
}

// ErrPermissionDenied matches every denial (errors.Is).
var ErrPermissionDenied = errors.New("permiso denegado")

type permissionDeniedError struct{ permission Permission }

func (e permissionDeniedError) Error() string {
	return "No tienes permiso para " + permissionActions[e.permission] + ". Pídele al dueño que te lo active."
}

func (e permissionDeniedError) Is(target error) bool { return target == ErrPermissionDenied }

// PermissionDeniedMessage is the Spanish message shown when p is denied.
func PermissionDeniedMessage(p Permission) string {
	return permissionDeniedError{permission: p}.Error()
}

// NewPermissionDeniedError is the 403 a use case returns when the actor lacks p.
func NewPermissionDeniedError(p Permission) CustomError {
	return CustomError{
		Err:       permissionDeniedError{permission: p},
		ErrorCode: CodeForbidden,
		Data:      map[string]any{"permission": string(p)},
	}
}

// Grants is what the authenticated user may do.
type Grants struct {
	owner bool
	set   map[Permission]bool
}

// OwnerGrants allows everything.
func OwnerGrants() Grants { return Grants{owner: true} }

// OperatorGrants allows only the listed permissions.
func OperatorGrants(perms []Permission) Grants {
	set := make(map[Permission]bool, len(perms))
	for _, p := range perms {
		set[p] = true
	}
	return Grants{set: set}
}

// Allows reports whether the grants include p.
func (g Grants) Allows(p Permission) bool { return g.owner || g.set[p] }

type grantsKey struct{}

// WithGrants stores the actor's grants in the request context. The
// operator-permissions middleware sets it on every write request with a
// bearer token.
func WithGrants(ctx context.Context, g Grants) context.Context {
	return context.WithValue(ctx, grantsKey{}, g)
}

// GrantsFrom returns the actor's grants, if the request set them.
func GrantsFrom(ctx context.Context) (Grants, bool) {
	g, ok := ctx.Value(grantsKey{}).(Grants)
	return g, ok
}

// RequirePermission returns the 403 error when the actor in ctx lacks p.
// A context without grants is a call that did not come from a user request
// (a job, a test, an internal flow) and is allowed: every HTTP write request
// carries grants because the middleware is installed on the whole engine.
func RequirePermission(ctx context.Context, p Permission) error {
	g, ok := GrantsFrom(ctx)
	if !ok || g.Allows(p) {
		return nil
	}
	return NewPermissionDeniedError(p)
}
