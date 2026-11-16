package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// RefreshSession (UC-002) trades a live refresh token for a new pair. The
// dashboard calls it when its 15-minute access token expires; without it the
// owner was sent back to the login screen every quarter of an hour.
//
// The refresh token rotates: the one presented stops working
// RefreshReuseGrace later, so a copy only lasts until its owner refreshes.
// The grace covers two tabs (or a duplicated tab) refreshing with the same
// token at once; without it the slower tab would send the owner to login.
// It honors logout (the token's own hash), a password reset or deactivation
// (RevokeAllForUser) and the account state, and it mints the role the user
// has now, not the one in the old token.
type RefreshSession struct {
	Users     userRepo.UserRepository
	Blacklist userRepo.RefreshTokenBlacklist
	UoW       sharedDomain.UnitOfWork
	Tokens    auth.TokenService
	NowFunc   func() time.Time
}

// RefreshReuseGrace is how long a rotated refresh token keeps working.
const RefreshReuseGrace = 30 * time.Second

func NewRefreshSession(users userRepo.UserRepository, bl userRepo.RefreshTokenBlacklist,
	uow sharedDomain.UnitOfWork, tokens auth.TokenService) *RefreshSession {
	return &RefreshSession{Users: users, Blacklist: bl, UoW: uow, Tokens: tokens,
		NowFunc: func() time.Time { return time.Now().UTC() }}
}

type RefreshSessionOutput struct {
	UserID       uuid.UUID
	GymID        uuid.UUID
	Role         string
	AccessToken  string
	RefreshToken string
}

func (uc *RefreshSession) Execute(ctx context.Context, refreshToken string) (RefreshSessionOutput, error) {
	invalid := sharedDomain.NewBusinessError(userErrors.ErrInvalidRefreshToken, "")
	if refreshToken == "" {
		return RefreshSessionOutput{}, invalid
	}
	claims, err := uc.Tokens.ValidateRefreshToken(refreshToken)
	if err != nil {
		return RefreshSessionOutput{}, invalid
	}
	hash, err := uc.Tokens.HashRefreshToken(refreshToken)
	if err != nil {
		return RefreshSessionOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	ctx = sharedDomain.WithTenant(ctx, claims.GymID)
	var out RefreshSessionOutput
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		revoked, err := uc.Blacklist.IsRevoked(tx, hash)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if revoked {
			return invalid
		}
		allAt, err := uc.Blacklist.RevokedAllAt(tx, claims.UserID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// The JWT keeps whole seconds. A token from the same second as the
		// revocation stays valid so a login right after a password reset is
		// not thrown out.
		if allAt != nil && claims.IssuedAt.Before(allAt.UTC().Truncate(time.Second)) {
			return invalid
		}
		user, err := uc.Users.GetByID(tx, claims.UserID)
		if errors.Is(err, userErrors.ErrUserNotFound) {
			return invalid
		}
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if user.GymID != claims.GymID {
			return invalid
		}
		if !user.Active {
			return sharedDomain.NewBusinessError(userErrors.ErrAccountInactive, "")
		}
		now := uc.NowFunc()
		exp := claims.ExpiresAt
		if exp.IsZero() {
			exp = now.Add(auth.RefreshTokenDuration)
		}
		if err := uc.Blacklist.RevokeAt(tx, hash, user.ID, exp, now.Add(RefreshReuseGrace)); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if err := uc.Blacklist.DeleteExpiredForUser(tx, user.ID, now); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		access, err := uc.Tokens.GenerateAccessToken(user.ID, user.GymID, user.Role)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		refresh, err := uc.Tokens.GenerateRefreshToken(user.ID, user.GymID, user.Role)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		out = RefreshSessionOutput{UserID: user.ID, GymID: user.GymID, Role: user.Role,
			AccessToken: access, RefreshToken: refresh}
		return nil
	})
	if err != nil {
		return RefreshSessionOutput{}, err
	}
	return out, nil
}
