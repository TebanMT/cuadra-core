package app

import (
	"context"

	"github.com/google/uuid"

	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// OperatorForLogin is the public projection rendered on the sidecar's
// /auth/operators endpoint. Intentionally narrow: name, role, has_pin. We
// drop email, phone, last_login_at, must_change_password — everything that
// could help an attacker enumerate or social-engineer past the kiosk.
type OperatorForLogin struct {
	UserID   uuid.UUID `json:"user_id"`
	FullName string    `json:"full_name"`
	Role     string    `json:"role"`
	HasPIN   bool      `json:"has_pin"`
}

// ListOperatorsForLogin is the read model behind the sidecar's login grid.
// Used by the desktop's /auth/login page when the laptop is paired. Returns
// active users only — soft-deleted / deactivated operators don't show up on
// the kiosk.
//
// Owner is always returned regardless of has_pin so the first-launch UX
// (owner has redeemed installer but not yet set a PIN) shows the owner with
// a "configura tu PIN" CTA. Operators only show when has_pin = true; they
// have nothing to tap on otherwise.
type ListOperatorsForLogin struct {
	Users userRepo.UserRepository
	UoW   sharedDomain.UnitOfWork
}

func NewListOperatorsForLogin(users userRepo.UserRepository, uow sharedDomain.UnitOfWork) *ListOperatorsForLogin {
	return &ListOperatorsForLogin{Users: users, UoW: uow}
}

func (uc *ListOperatorsForLogin) Execute(ctx context.Context, gymID uuid.UUID) ([]OperatorForLogin, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, err
	}
	users, err := uc.Users.ListByGym(tx, gymID)
	if err != nil {
		return nil, err
	}
	out := make([]OperatorForLogin, 0, len(users))
	for _, u := range users {
		if !u.Active {
			continue
		}
		// Operators without a PIN can't authenticate via the kiosk grid,
		// so omit them. Owners are always rendered (the grid shows them
		// with a "configura tu PIN" badge when has_pin is false).
		if !u.IsOwner() && !u.HasPIN() {
			continue
		}
		out = append(out, OperatorForLogin{
			UserID:   u.ID,
			FullName: u.FullName,
			Role:     u.Role,
			HasPIN:   u.HasPIN(),
		})
	}
	return out, nil
}
