package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	alertDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/alertconfig"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	usersRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// OwnerAlertKind aliases the canonical alertconfig.Key — keeping the
// trigger vocabulary identical to the toggle vocabulary lets the dispatch
// path consult the config repo directly with no translation step.
type OwnerAlertKind = alertDomain.Key

const (
	OwnerAlertLowStock         = alertDomain.KeyLowStock
	OwnerAlertExpiredNoContact = alertDomain.KeyExpiredNoContact
	OwnerAlertVIPMemberNoVisit = alertDomain.KeyVIPMemberNoVisit
	OwnerAlertCashCloseDiff    = alertDomain.KeyCashCloseDiff
	OwnerAlertNoPaymentsToday  = alertDomain.KeyNoPaymentsToday
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
//
// The Configs repo is consulted before each enqueue: when the owner has
// disabled the alert key we skip silently (Skipped=true,
// SkippedReason="alert_disabled"). When Configs is nil — older callers,
// tests — gating is bypassed and every kind enqueues, so callers that
// haven't migrated yet keep their existing behaviour.
type EnqueueOwnerAlert struct {
	Notifications notiRepo.NotificationRepository
	Gyms          gymRepo.GymRepository
	Users         usersRepo.UserRepository
	Configs       notiRepo.AlertConfigRepository
	UoW           sharedDomain.UnitOfWork
}

func NewEnqueueOwnerAlert(
	notifications notiRepo.NotificationRepository,
	gyms gymRepo.GymRepository,
	users usersRepo.UserRepository,
	configs notiRepo.AlertConfigRepository,
	uow sharedDomain.UnitOfWork,
) *EnqueueOwnerAlert {
	return &EnqueueOwnerAlert{
		Notifications: notifications,
		Gyms:          gyms,
		Users:         users,
		Configs:       configs,
		UoW:           uow,
	}
}

func (uc *EnqueueOwnerAlert) Execute(ctx context.Context, in EnqueueOwnerAlertInput) (*EnqueueOwnerAlertOutput, error) {
	now := time.Now().UTC()
	out := EnqueueOwnerAlertOutput{}

	templateKey, ok := OwnerAlertTemplateKey(in.Kind)
	if !ok {
		out.Skipped = true
		out.SkippedReason = "unknown_alert_kind"
		return &out, nil
	}

	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		if uc.Configs != nil {
			enabled, err := IsOwnerAlertEnabled(tx, uc.Configs, in.GymID, in.Kind)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if !enabled {
				out.Skipped = true
				out.SkippedReason = "alert_disabled"
				return nil
			}
		}
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
		// ADR-009: no WhatsApp-connection gate — the dispatcher resolves the
		// sender by tier (Tinta master for Standard, gym's own number for
		// connected Plus). We only need the owner to have a phone.
		phone := ownerPhone
		if phone == "" {
			out.Skipped = true
			out.SkippedReason = "no_owner_phone"
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

		// El sufijo default (la fecha del día) usa el día LOCAL del gym:
		// con la fecha UTC, la "alerta diaria" cambiaba de llave a las
		// 6 PM de CDMX y el dueño podía recibirla DOS veces el mismo día.
		localDay := tz.LocalToday(gym.Timezone, now).Format("2006-01-02")
		idempKey := fmt.Sprintf("owner_alert:%s:%s", in.Kind, defaultStr(in.IdempotencyKeySuffix, localDay))
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

// OwnerAlertTemplateKey resolves the WhatsApp template key for a canonical
// AlertKey. Every AlertKey in alertconfig.Defaults must have a matching
// template — the default branch is a defensive guard for enum drift.
func OwnerAlertTemplateKey(k OwnerAlertKind) (string, bool) {
	switch k {
	case OwnerAlertLowStock:
		return "owner_alert_low_stock", true
	case OwnerAlertExpiredNoContact:
		return "owner_alert_expired_batch", true
	case OwnerAlertCashCloseDiff:
		return "owner_alert_cash_close_diff", true
	case OwnerAlertVIPMemberNoVisit:
		return "owner_alert_vip_no_visit", true
	case OwnerAlertNoPaymentsToday:
		return "owner_alert_no_payments_today", true
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
