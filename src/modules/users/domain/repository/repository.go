// Package repository declares the persistence contract for users + auth-side
// tokens. Concrete impls live in infraestructure/db/repositories.
package repository

import (
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/users/domain/legal"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type UserRepository interface {
	Create(tx sharedDomain.Transaction, u *userDomain.User) (*userDomain.User, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*userDomain.User, error)
	GetByEmail(tx sharedDomain.Transaction, email string) (*userDomain.User, error)
	ExistsByEmail(tx sharedDomain.Transaction, email string) (bool, error)
	// ExistsByPhoneInGym is the cheap check used by CreateOperator /
	// UpdateOperator before persisting. excludeUserID lets the caller
	// pass the row being edited so an update that keeps the same number
	// doesn't trigger a false collision. The repo normalizes the phone
	// the same way the domain does (userDomain.NormalizePhone) so callers
	// can pass user-typed input directly.
	ExistsByPhoneInGym(tx sharedDomain.Transaction, gymID uuid.UUID, phone string, excludeUserID *uuid.UUID) (bool, error)
	Update(tx sharedDomain.Transaction, u *userDomain.User) (*userDomain.User, error)
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*userDomain.User, error)
	// CountOperatorsByGym cuenta los operadores ACTIVOS y no borrados del gym
	// (el cupo del plan). Ni el dueño ni los operadores desactivados cuentan.
	CountOperatorsByGym(tx sharedDomain.Transaction, gymID uuid.UUID) (int, error)
}

// PasswordResetRepository persists single-use reset tokens (UC-004). The repo
// stores the SHA-256 hash, never the plaintext.
type PasswordResetRepository interface {
	Save(tx sharedDomain.Transaction, rec PasswordResetRecord) error
	Consume(tx sharedDomain.Transaction, plainToken string, now time.Time) (uuid.UUID, error)
}

type PasswordResetRecord struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	PlainToken string // only present when issuing — the repo hashes it
	TokenHash  []byte
	ExpiresAt  time.Time
}

// EmailVerificationRepository persists single-use verification tokens
// (UC-013). Same shape as PasswordResetRepository — separate table so a
// pending password-reset can't be redeemed as a verification or vice
// versa, even if a token string somehow collided.
type EmailVerificationRepository interface {
	Save(tx sharedDomain.Transaction, rec EmailVerificationRecord) error
	Consume(tx sharedDomain.Transaction, plainToken string, now time.Time) (uuid.UUID, error)
}

type EmailVerificationRecord struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	PlainToken string // only present when issuing — the repo hashes it
	TokenHash  []byte
	ExpiresAt  time.Time
}

// RefreshTokenBlacklist tracks revoked refresh tokens (UC-003 logout, UC-004
// invalidate-all-sessions).
type RefreshTokenBlacklist interface {
	Revoke(tx sharedDomain.Transaction, tokenHash []byte, userID uuid.UUID, expiresAt time.Time) error
	// RevokeAt revokes the token from revokedAt on. Refresh rotation uses a
	// moment a few seconds ahead so two tabs refreshing at once both work.
	// An earlier revocation of the same token wins.
	RevokeAt(tx sharedDomain.Transaction, tokenHash []byte, userID uuid.UUID, expiresAt, revokedAt time.Time) error
	IsRevoked(tx sharedDomain.Transaction, tokenHash []byte) (bool, error)
	RevokeAllForUser(tx sharedDomain.Transaction, userID uuid.UUID, expiresAt time.Time) error
	// RevokedAllAt returns the moment of the last RevokeAllForUser for the
	// user, or nil when none is active. Refresh tokens issued before it are
	// revoked.
	RevokedAllAt(tx sharedDomain.Transaction, userID uuid.UUID) (*time.Time, error)
	// DeleteExpiredForUser drops the user's rows whose token already expired:
	// an expired refresh token is rejected anyway, so the row blocks nothing.
	// Refresh rotation calls it to keep the table bounded.
	DeleteExpiredForUser(tx sharedDomain.Transaction, userID uuid.UUID, now time.Time) error
}

// LegalAcceptanceRepository stores the owner's acceptance of the Terms of
// Service and the Privacy Notice (UC-001). Cloud only; rows are append-only
// evidence and are never updated.
type LegalAcceptanceRepository interface {
	Record(tx sharedDomain.Transaction, acceptances []legal.Acceptance) error
}
