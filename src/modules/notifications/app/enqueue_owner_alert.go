package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	usersRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// OwnerAlertKind enumerates the trigger types covered by UC-040. New kinds
// can be added without API changes — the variable map is the contract.
type OwnerAlertKind string

const (
	OwnerAlertLowStock        OwnerAlertKind = "low_stock"
	OwnerAlertExpiredBatch    OwnerAlertKind = "expired_batch"
	OwnerAlertCashCloseDiff   OwnerAlertKind = "cash_close_diff"
	OwnerAlertNoCobrosToday   OwnerAlertKind = "no_cobros_today"
	OwnerAlertVIPNotPresent   OwnerAlertKind = "vip_not_present"
)

// EnqueueOwnerAlertInput is what the cron / event hook hands to the use
// case. Vars must include the keys the matching template expects.
type EnqueueOwnerAlertInput struct {
	GymID uuid.UUID
	Kind  OwnerAlertKind
	Vars  map[string]string

	// IdempotencyKeySuffix is appended to the alert kind to deduplicate
	// repeated triggers (e.g. "stock_low:product_id:date").
	IdempotencyKeySuffix string
}

type EnqueueOwnerAlertOutput struct {
	Skipped        bool
	SkippedReason  string
	NotificationID *uuid.UUID
}

// EnqueueOwnerAlert is UC-040. The owner is a User row with role=owner —
// we look up the gym's owner and enqueue a WhatsApp message to them.
type EnqueueOwnerAlert struct {
	Notifications notiRepo.NotificationRepository
	Gyms          gymRepo.GymRepository
	Users         usersRepo.UserRepository
	UoW           sharedDomain.UnitOfWork
}

func NewEnqueueOwnerAlert(
	notifications notiRepo.NotificationRepository,
	gyms gymRepo.GymRepository,
	users usersRepo.UserRepository,
	uow sharedDomain.UnitOfWork,
) *EnqueueOwnerAlert {
	return &EnqueueOwnerAlert{Notifications: notifications, Gyms: gyms, Users: users, UoW: uow}
}

func (uc *EnqueueOwnerAlert) Execute(ctx context.Context, in EnqueueOwnerAlertInput) (*EnqueueOwnerAlertOutput, error) {
	now := time.Now().UTC()
	out := EnqueueOwnerAlertOutput{}

	templateKey, ok := ownerAlertTemplate(in.Kind)
	if !ok {
		out.Skipped = true
		out.SkippedReason = "unknown_alert_kind"
		return &out, nil
	}

	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		gym, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return err
		}
		users, err := uc.Users.ListByGym(tx, in.GymID)
		if err != nil {
			return err
		}
		var owner *uuid.UUID
		ownerPhone := ""
		for _, u := range users {
			if u.Role != "owner" || !u.Active {
				continue
			}
			id := u.ID
			owner = &id
			if u.Phone != nil {
				ownerPhone = strings.TrimSpace(*u.Phone)
			}
			break
		}
		if owner == nil {
			out.Skipped = true
			out.SkippedReason = "no_owner"
			return nil
		}
		phone := ownerPhone
		if !gym.IsWhatsAppConnected() || phone == "" {
			out.Skipped = true
			out.SkippedReason = "no_whatsapp_or_phone"
			return nil
		}
		gymName := ""
		if gym.Name != nil {
			gymName = *gym.Name
		}
		vars := map[string]string{"gym_name": gymName}
		for k, v := range in.Vars {
			vars[k] = v
		}

		idempKey := fmt.Sprintf("owner_alert:%s:%s", in.Kind, defaultStr(in.IdempotencyKeySuffix, now.Format("2006-01-02")))
		existing, err := uc.Notifications.GetByIdempotencyKey(tx, in.GymID, idempKey)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if existing != nil {
			id := existing.ID
			out.NotificationID = &id
			return nil
		}

		n, err := notiDomain.New(
			uuid.New(), in.GymID, *owner,
			notiDomain.ChannelWhatsApp,
			templateKey,
			notiDomain.RecipientUser,
			phone,
			vars,
			now, now,
			&idempKey,
		)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		saved, err := uc.Notifications.Create(tx, n)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		id := saved.ID
		out.NotificationID = &id
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func ownerAlertTemplate(k OwnerAlertKind) (string, bool) {
	switch k {
	case OwnerAlertLowStock:
		return "owner_alert_low_stock", true
	case OwnerAlertExpiredBatch:
		return "owner_alert_expired_batch", true
	case OwnerAlertCashCloseDiff:
		return "owner_alert_cash_close_diff", true
	default:
		return "", false
	}
}

func defaultStr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
