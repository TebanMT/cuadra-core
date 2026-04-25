package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Logout (UC-003). Best-effort: when offline the sidecar drops the local
// tokens; cloud invalidates the refresh token. Silent no-op if the refresh
// token can't be parsed.
type Logout struct {
	Blacklist userRepo.RefreshTokenBlacklist
	UoW       sharedDomain.UnitOfWork
	Tokens    auth.TokenService
	Audit     audit.Recorder
}

func NewLogout(bl userRepo.RefreshTokenBlacklist, uow sharedDomain.UnitOfWork, tokens auth.TokenService, recorder audit.Recorder) *Logout {
	return &Logout{Blacklist: bl, UoW: uow, Tokens: tokens, Audit: recorder}
}

type LogoutInput struct {
	RefreshToken string
	UserID       uuid.UUID
	GymID        uuid.UUID
}

func (uc *Logout) Execute(ctx context.Context, in LogoutInput) error {
	if in.RefreshToken == "" {
		return nil
	}
	claims, err := uc.Tokens.ValidateRefreshToken(in.RefreshToken)
	if err != nil {
		return nil // already invalid — treat logout as success
	}
	hash, err := uc.Tokens.HashRefreshToken(in.RefreshToken)
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	exp := claims.ExpiresAt
	if exp.IsZero() {
		exp = time.Now().UTC().Add(auth.RefreshTokenDuration)
	}
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		if err := uc.Blacklist.Revoke(tx, hash, claims.UserID, exp); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       claims.GymID,
			EntityType:  "users",
			EntityID:    claims.UserID,
			Action:      audit.ActionLogout,
			ActorUserID: &claims.UserID,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          time.Now().UTC(),
		})
		return nil
	})
}
