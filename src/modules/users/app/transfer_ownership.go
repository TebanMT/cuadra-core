package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/email"
)

// TransferOwnershipOTPTTL = 10 min (UC-010 datos persistidos: "código de 6 dígitos válido 10 minutos").
const TransferOwnershipOTPTTL = 10 * time.Minute

// RequestTransferOwnership is UC-010 step 1 — generate OTP, email it.
type RequestTransferOwnership struct {
	Users   userRepo.UserRepository
	OTPs    gymRepo.OwnershipOTPRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
	Email   email.Sender
	NowFunc func() time.Time
}

func NewRequestTransferOwnership(users userRepo.UserRepository, otps gymRepo.OwnershipOTPRepository,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder, sender email.Sender) *RequestTransferOwnership {
	return &RequestTransferOwnership{
		Users: users, OTPs: otps, UoW: uow, Audit: recorder, Email: sender,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type RequestTransferOwnershipInput struct {
	GymID    uuid.UUID
	OwnerID  uuid.UUID
	TargetID uuid.UUID
}

func (uc *RequestTransferOwnership) Execute(ctx context.Context, in RequestTransferOwnershipInput) error {
	if in.OwnerID == in.TargetID {
		return sharedDomain.NewBusinessError(userErrors.ErrTransferToSelf, "")
	}
	now := uc.NowFunc()
	code, err := auth.GenerateOTPCode(6)
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}

	var ownerEmail string
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		owner, err := uc.Users.GetByID(tx, in.OwnerID)
		if err != nil {
			return err
		}
		if owner.GymID != in.GymID || !owner.IsOwner() {
			return sharedDomain.NewBusinessError(userErrors.ErrInvalidToken, "")
		}
		target, err := uc.Users.GetByID(tx, in.TargetID)
		if err != nil {
			return sharedDomain.NewBusinessError(userErrors.ErrTargetNotEligible, "")
		}
		if target.GymID != in.GymID || !target.IsOperator() || !target.Active {
			return sharedDomain.NewBusinessError(userErrors.ErrTargetNotEligible, "")
		}
		rec := gymRepo.OTPRecord{
			ID:         uuid.New(),
			GymID:      in.GymID,
			FromUserID: in.OwnerID,
			ToUserID:   in.TargetID,
			PlainCode:  code,
			ExpiresAt:  now.Add(TransferOwnershipOTPTTL).Unix(),
		}
		if err := uc.OTPs.Save(tx, rec); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "gyms",
			EntityID:    in.GymID,
			Action:      audit.ActionTransferOwnership + ":requested",
			ActorUserID: &in.OwnerID,
			Changes:     map[string]any{"to_user_id": in.TargetID.String()},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		ownerEmail = owner.Email
		return nil
	})
	if err != nil {
		return err
	}
	_ = uc.Email.Send(ctx, email.Message{
		To:      ownerEmail,
		Subject: "Confirma la transferencia de tu gimnasio",
		Body:    "Código: " + code + " (válido 10 minutos)",
		Tag:     "transfer_ownership_otp",
	})
	return nil
}

// ConfirmTransferOwnership is UC-010 step 2.
type ConfirmTransferOwnership struct {
	Users     userRepo.UserRepository
	OTPs      gymRepo.OwnershipOTPRepository
	Transfers gymRepo.OwnershipTransferRepository
	Blacklist userRepo.RefreshTokenBlacklist
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
	Email     email.Sender
	NowFunc   func() time.Time
}

func NewConfirmTransferOwnership(users userRepo.UserRepository, otps gymRepo.OwnershipOTPRepository,
	transfers gymRepo.OwnershipTransferRepository, bl userRepo.RefreshTokenBlacklist,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder, sender email.Sender) *ConfirmTransferOwnership {
	return &ConfirmTransferOwnership{
		Users: users, OTPs: otps, Transfers: transfers, Blacklist: bl,
		UoW: uow, Audit: recorder, Email: sender,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type ConfirmTransferOwnershipInput struct {
	GymID   uuid.UUID
	OwnerID uuid.UUID
	Code    string
}

type ConfirmTransferOwnershipOutput struct {
	NewOwnerID uuid.UUID
	OldOwnerID uuid.UUID
	TransferID uuid.UUID
	ExecutedAt time.Time
}

func (uc *ConfirmTransferOwnership) Execute(ctx context.Context, in ConfirmTransferOwnershipInput) (ConfirmTransferOwnershipOutput, error) {
	now := uc.NowFunc()
	var out ConfirmTransferOwnershipOutput
	var newOwnerEmail string
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		rec, err := uc.OTPs.Consume(tx, in.GymID, in.OwnerID, in.Code)
		if err != nil {
			return err
		}
		oldOwner, err := uc.Users.GetByID(tx, in.OwnerID)
		if err != nil {
			return err
		}
		if oldOwner.GymID != in.GymID || !oldOwner.IsOwner() {
			return sharedDomain.NewBusinessError(userErrors.ErrInvalidToken, "")
		}
		newOwner, err := uc.Users.GetByID(tx, rec.ToUserID)
		if err != nil {
			return err
		}
		if newOwner.GymID != in.GymID || !newOwner.IsOperator() || !newOwner.Active {
			return sharedDomain.NewBusinessError(userErrors.ErrTargetNotEligible, "")
		}
		// Demote first to free the partial unique index uq_users_gym_owner.
		oldOwner.DemoteToOperator(now)
		if _, err := uc.Users.Update(tx, oldOwner); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		newOwner.PromoteToOwner(now)
		newOwner.Role = userDomain.RoleOwner // explicit guard for readability
		if _, err := uc.Users.Update(tx, newOwner); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		transferID, err := uc.Transfers.RecordTransfer(tx, in.GymID, oldOwner.ID, newOwner.ID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// Invalidate the old owner's refresh tokens — their permissions changed.
		if uc.Blacklist != nil {
			if err := uc.Blacklist.RevokeAllForUser(tx, oldOwner.ID, now.Add(auth.RefreshTokenDuration)); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "gyms",
			EntityID:    in.GymID,
			Action:      audit.ActionTransferOwnership,
			ActorUserID: &in.OwnerID,
			Changes:     map[string]any{"from": oldOwner.ID.String(), "to": newOwner.ID.String()},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = ConfirmTransferOwnershipOutput{
			NewOwnerID: newOwner.ID,
			OldOwnerID: oldOwner.ID,
			TransferID: transferID,
			ExecutedAt: now,
		}
		newOwnerEmail = newOwner.Email
		return nil
	})
	if err != nil {
		return ConfirmTransferOwnershipOutput{}, err
	}
	if newOwnerEmail != "" {
		_ = uc.Email.Send(ctx, email.Message{
			To:      newOwnerEmail,
			Subject: "Eres el nuevo dueño de tu gimnasio",
			Body:    "Ya tienes control total. Inicia sesión para administrar.",
			Tag:     "transfer_ownership_complete",
		})
	}
	return out, nil
}
