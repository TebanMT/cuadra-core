// Package repository declares the persistence contract for users + auth-side
// tokens. Concrete impls live in infraestructure/db/repositories.
package repository

import (
	"time"

	"github.com/google/uuid"

	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type UserRepository interface {
	Create(tx sharedDomain.Transaction, u *userDomain.User) (*userDomain.User, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*userDomain.User, error)
	GetByEmail(tx sharedDomain.Transaction, email string) (*userDomain.User, error)
	ExistsByEmail(tx sharedDomain.Transaction, email string) (bool, error)
	Update(tx sharedDomain.Transaction, u *userDomain.User) (*userDomain.User, error)
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*userDomain.User, error)
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

// RefreshTokenBlacklist tracks revoked refresh tokens (UC-003 logout, UC-004
// invalidate-all-sessions).
type RefreshTokenBlacklist interface {
	Revoke(tx sharedDomain.Transaction, tokenHash []byte, userID uuid.UUID, expiresAt time.Time) error
	IsRevoked(tx sharedDomain.Transaction, tokenHash []byte) (bool, error)
	RevokeAllForUser(tx sharedDomain.Transaction, userID uuid.UUID, expiresAt time.Time) error
}
