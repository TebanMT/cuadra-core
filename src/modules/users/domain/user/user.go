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
	// PinHash is bcrypt of the 4-digit reception PIN. Nil = no PIN. Distinct
	// from PasswordHash because PIN login is a separate, weaker channel
	// (offline-validatable on the sidecar) that we want to be able to add
	// or revoke independently of the operator's web password.
	PinHash       *string
	PinAssignedAt *time.Time
}

// NewUser constructs a User with role + canonicalised email. Caller is
// responsible for hashing the password (shared/auth.HashPassword) and for
// validating phone via ValidatePhone before calling SetInitialPhone.
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

// SetInitialPhone stores phone at create time without bumping Version (the
// user is still v1). Empty / whitespace-only resets to nil.
func (u *User) SetInitialPhone(phone string) {
	v := strings.TrimSpace(phone)
	if v == "" {
		u.Phone = nil
		return
	}
	u.Phone = &v
}

// HasPIN reports whether the user currently has a reception PIN set. Used by
// the operators-list endpoint so the desktop's login grid can decide which
// avatars get a "tap to enter" CTA vs. a "configura tu PIN" badge.
func (u *User) HasPIN() bool { return u.PinHash != nil && *u.PinHash != "" }

// AssignPIN hashes + stores the given 4-digit PIN. Caller passes the bcrypt
// hash (shared/auth.HashPIN owns the hashing) so this method is purely the
// domain bookkeeping: version bump, timestamp, mutated state. Pass empty
// hash + ValidatePIN failure handled at the use-case layer.
func (u *User) AssignPIN(hash string, now time.Time) {
	u.PinHash = &hash
	u.PinAssignedAt = &now
	u.Version++
	u.UpdatedAt = now
}

// ClearPIN revokes the PIN. Same audit posture as AssignPIN.
func (u *User) ClearPIN(now time.Time) {
	u.PinHash = nil
	u.PinAssignedAt = nil
	u.Version++
	u.UpdatedAt = now
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
			if err := ValidatePhone(v); err != nil {
				return err
			}
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

// ValidateFullName enforces 3..100 (UC-001 step 1) and rejects email-shaped
// strings — early on we had reports of operators registering with their email
// as their name, which then surfaced as "Hola, foo@bar.com" in the dashboard.
// A full name has no @ in it; treat that as a clear signal the user pasted
// the wrong field.
func ValidateFullName(name string) error {
	v := strings.TrimSpace(name)
	if len(v) < 3 || len(v) > 100 {
		return userErrors.ErrNameRequired
	}
	if strings.ContainsRune(v, '@') {
		return userErrors.ErrNameLooksLikeEmail
	}
	return nil
}

// ValidatePIN enforces the wire shape of a reception PIN. Exactly 4 ASCII
// digits — the PinPad component on the desktop is locked to that length so
// we mirror it on the server. We deliberately don't reject "1234"-style
// weak PINs at the domain edge: the desktop is a kiosk in a small gym, the
// owner already controls who's near the keypad, and forbidding obvious
// PINs would just push owners to write the PIN on a Post-it.
func ValidatePIN(pin string) error {
	if len(pin) != 4 {
		return userErrors.ErrInvalidPIN
	}
	for _, r := range pin {
		if r < '0' || r > '9' {
			return userErrors.ErrInvalidPIN
		}
	}
	return nil
}

// ValidatePhone is intentionally lax — the segment (gym owners in MX) types
// "55 1234 5678", "+52 442 123 4567", "4421234567", whatever feels natural.
// We don't reach for E.164: signup must not fail because the field rejected
// a perfectly fine local number. Rules are minimal:
//   - 7..20 chars total after trimming (covers shortest fixed-line up to
//     longest international with separators)
//   - At least 7 digits when separators are stripped (rejects "abc-def" /
//     pure-symbols garbage)
//   - Only digits, spaces, '+', '-', '(', ')', '.' allowed
//
// Phone is optional everywhere, so callers should only invoke this when the
// trimmed value is non-empty.
func ValidatePhone(phone string) error {
	v := strings.TrimSpace(phone)
	if len(v) < 7 || len(v) > 20 {
		return userErrors.ErrInvalidPhone
	}
	digits := 0
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == ' ' || r == '+' || r == '-' || r == '(' || r == ')' || r == '.':
			// allowed separator
		default:
			return userErrors.ErrInvalidPhone
		}
	}
	if digits < 7 {
		return userErrors.ErrInvalidPhone
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
