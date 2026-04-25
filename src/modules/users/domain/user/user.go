// Package user holds the User aggregate (operator / owner of a gym). All
// password handling is done via shared/auth — this entity only holds the hash.
package user

import (
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
)

const (
	RoleOwner    = "owner"
	RoleOperator = "operator"

	// MaxOperatorsPerGym enforces UC-006 DA-6.2 (hard cap of 10 operators).
	// Includes the owner.
	MaxOperatorsPerGym = 11
)

// User is the gym operator / owner aggregate. Email is canonicalised to lower
// case (the unique index is LOWER(email)).
type User struct {
	ID                 uuid.UUID
	GymID              uuid.UUID
	Version            int
	Email              string
	PasswordHash       string
	FullName           string
	Phone              *string
	Role               string
	Active             bool
	MustChangePassword bool
	LastLoginAt        *time.Time
	CreatedBy          *uuid.UUID
	CreatedAt          time.Time
	UpdatedAt          time.Time
	DeletedAt          *time.Time
}

// NewUser constructs a User with role + canonicalised email. Caller is
// responsible for hashing the password (shared/auth.HashPassword).
func NewUser(id, gymID uuid.UUID, email, passwordHash, fullName, role string, mustChange bool, createdBy *uuid.UUID, now time.Time) *User {
	return &User{
		ID:                 id,
		GymID:              gymID,
		Version:            1,
		Email:              strings.ToLower(strings.TrimSpace(email)),
		PasswordHash:       passwordHash,
		FullName:           strings.TrimSpace(fullName),
		Role:               role,
		Active:             true,
		MustChangePassword: mustChange,
		CreatedBy:          createdBy,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

func (u *User) IsOwner() bool    { return u.Role == RoleOwner }
func (u *User) IsOperator() bool { return u.Role == RoleOperator }

// MarkLoggedIn bumps last_login_at. Called from UC-002 after credentials check.
func (u *User) MarkLoggedIn(now time.Time) {
	u.LastLoginAt = &now
	u.Version++
	u.UpdatedAt = now
}

// ApplyPassword hashes & assigns. Use shared/auth.HashPassword + this together.
func (u *User) ApplyPassword(hash string, mustChange bool, now time.Time) {
	u.PasswordHash = hash
	u.MustChangePassword = mustChange
	u.Version++
	u.UpdatedAt = now
}

// SetActive toggles active. Returns errors when the rule is violated; the
// caller (UC-008) catches them and maps to user-friendly responses.
func (u *User) SetActive(active bool, callerID uuid.UUID, now time.Time) error {
	if u.ID == callerID {
		return userErrors.ErrSelfDeactivate
	}
	if u.IsOwner() && !active {
		return userErrors.ErrCannotDeactivateOwner
	}
	if u.Active == active {
		return nil
	}
	u.Active = active
	u.Version++
	u.UpdatedAt = now
	return nil
}

// PromoteToOwner / DemoteToOperator are used by UC-010. Called inside the same
// UoW.Command so the unique index uq_users_gym_owner can briefly see two
// owners; Postgres allows this within a single statement plan but we tolerate
// no overlap by ordering: demote first, promote second.
func (u *User) PromoteToOwner(now time.Time) {
	u.Role = RoleOwner
	u.Version++
	u.UpdatedAt = now
}

func (u *User) DemoteToOperator(now time.Time) {
	u.Role = RoleOperator
	u.Version++
	u.UpdatedAt = now
}

// UpdateProfile applies UC-007's editable fields. Only sets provided pointers.
func (u *User) UpdateProfile(name *string, email *string, phone *string, now time.Time) error {
	if name != nil {
		v := strings.TrimSpace(*name)
		if v == "" || len(v) > 100 {
			return userErrors.ErrNameRequired
		}
		u.FullName = v
	}
	if email != nil {
		v := strings.ToLower(strings.TrimSpace(*email))
		if !ValidateEmail(v) {
			return userErrors.ErrInvalidEmail
		}
		u.Email = v
	}
	if phone != nil {
		v := strings.TrimSpace(*phone)
		if v == "" {
			u.Phone = nil
		} else {
			u.Phone = &v
		}
	}
	u.Version++
	u.UpdatedAt = now
	return nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

var emailRegex = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// ValidateEmail returns true when the address is structurally valid; we don't
// do MX lookups (boring SaaS, §1.2 principle 7).
func ValidateEmail(email string) bool {
	if email == "" || len(email) > 254 {
		return false
	}
	return emailRegex.MatchString(email)
}

// ValidatePassword enforces the >=8 chars rule (UC-001 step 1). We deliberately
// don't require numbers/symbols (USE-CASES "no exigir números/símbolos en MVP").
func ValidatePassword(password string) error {
	if len(password) < 8 {
		return userErrors.ErrPasswordTooShort
	}
	return nil
}

// ValidateFullName enforces 3..100 (UC-001 step 1).
func ValidateFullName(name string) error {
	v := strings.TrimSpace(name)
	if len(v) < 3 || len(v) > 100 {
		return userErrors.ErrNameRequired
	}
	return nil
}

// ValidateRole guards the chk_users_role constraint at the domain edge so we
// fail fast with a friendly message before touching the DB.
func ValidateRole(role string) error {
	switch role {
	case RoleOwner, RoleOperator:
		return nil
	}
	return userErrors.ErrInvalidRole
}

// Validator is the chain interface for create-time validation.
type Validator interface {
	Validate(u *User) error
}

type emailValidator struct{ Next Validator }

func (v *emailValidator) Validate(u *User) error {
	if !ValidateEmail(u.Email) {
		return userErrors.ErrInvalidEmail
	}
	if v.Next != nil {
		return v.Next.Validate(u)
	}
	return nil
}

type nameValidator struct{ Next Validator }

func (v *nameValidator) Validate(u *User) error {
	if err := ValidateFullName(u.FullName); err != nil {
		return err
	}
	if v.Next != nil {
		return v.Next.Validate(u)
	}
	return nil
}

type roleValidator struct{ Next Validator }

func (v *roleValidator) Validate(u *User) error {
	if err := ValidateRole(u.Role); err != nil {
		return err
	}
	if v.Next != nil {
		return v.Next.Validate(u)
	}
	return nil
}

func BuildValidatorChain() Validator {
	return &emailValidator{Next: &nameValidator{Next: &roleValidator{}}}
}
