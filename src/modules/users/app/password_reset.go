package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/google/uuid"

	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/email"
)

// ResetTokenTTL = 1 hour (UC-004 datos persistidos).
const ResetTokenTTL = time.Hour

// RequestPasswordReset is UC-004 step 1 (POST /api/v1/auth/forgot-password).
// Always returns nil — the response shape is identical regardless of whether
// the email exists, so callers can't enumerate accounts.
type RequestPasswordReset struct {
	Users   userRepo.UserRepository
	Resets  userRepo.PasswordResetRepository
	UoW     sharedDomain.UnitOfWork
	Email   email.Sender
	Audit   audit.Recorder
	NowFunc func() time.Time
	BaseURL string // e.g. https://entinta.mx — used to build the reset link
}

func NewRequestPasswordReset(users userRepo.UserRepository, resets userRepo.PasswordResetRepository,
	uow sharedDomain.UnitOfWork, sender email.Sender, recorder audit.Recorder, baseURL string) *RequestPasswordReset {
	return &RequestPasswordReset{
		Users: users, Resets: resets, UoW: uow,
		Email: sender, Audit: recorder, BaseURL: baseURL,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

func (uc *RequestPasswordReset) Execute(ctx context.Context, emailAddr string) error {
	if !userDomain.ValidateEmail(emailAddr) {
		return sharedDomain.NewValidationError(userErrors.ErrInvalidEmail)
	}
	now := uc.NowFunc()
	plain, err := generateResetToken()
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}

	var sendTo string
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		u, err := uc.Users.GetByEmail(tx, emailAddr)
		if err != nil {
			// Don't reveal absence — silently succeed.
			return nil
		}
		rec := userRepo.PasswordResetRecord{
			ID:         uuid.New(),
			UserID:     u.ID,
			PlainToken: plain,
			ExpiresAt:  now.Add(ResetTokenTTL),
		}
		if err := uc.Resets.Save(tx, rec); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       u.GymID,
			EntityType:  "users",
			EntityID:    u.ID,
			Action:      audit.ActionPasswordResetReq,
			ActorUserID: &u.ID,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		sendTo = u.Email
		return nil
	})
	if err != nil {
		return err
	}
	if sendTo != "" {
		_ = uc.Email.Send(ctx, email.Message{
			To:      sendTo,
			Subject: "Recupera tu contraseña de Cuadra",
			Body:    uc.BaseURL + "/reset-password?token=" + plain,
			Tag:     "password_reset",
		})
	}
	return nil
}

// ConfirmPasswordReset is UC-004 step 2 (POST /api/v1/auth/reset-password).
type ConfirmPasswordReset struct {
	Users     userRepo.UserRepository
	Resets    userRepo.PasswordResetRepository
	Blacklist userRepo.RefreshTokenBlacklist
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
	NowFunc   func() time.Time
}

func NewConfirmPasswordReset(users userRepo.UserRepository, resets userRepo.PasswordResetRepository,
	bl userRepo.RefreshTokenBlacklist, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *ConfirmPasswordReset {
	return &ConfirmPasswordReset{
		Users: users, Resets: resets, Blacklist: bl, UoW: uow,
		Audit: recorder, NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

func (uc *ConfirmPasswordReset) Execute(ctx context.Context, token, newPassword string) error {
	if err := userDomain.ValidatePassword(newPassword); err != nil {
		return sharedDomain.NewValidationError(err)
	}
	if token == "" {
		return sharedDomain.NewBusinessError(userErrors.ErrInvalidResetToken, "")
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	now := uc.NowFunc()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		userID, err := uc.Resets.Consume(tx, token, now)
		if err != nil {
			return err
		}
		u, err := uc.Users.GetByID(tx, userID)
		if err != nil {
			return err
		}
		u.ApplyPassword(hash, false, now)
		if _, err := uc.Users.Update(tx, u); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// DA-4.2: invalidate every refresh token issued before now.
		if err := uc.Blacklist.RevokeAllForUser(tx, u.ID, now.Add(auth.RefreshTokenDuration)); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       u.GymID,
			EntityType:  "users",
			EntityID:    u.ID,
			Action:      audit.ActionPasswordReset,
			ActorUserID: &u.ID,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
}

// generateResetToken returns a 32-char hex string (128 bits of entropy).
func generateResetToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
