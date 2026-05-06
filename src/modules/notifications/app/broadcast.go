package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// BroadcastFilter selects the audience. MVP supports the two filters listed
// in UC-041 (DA-41.1) — anything more sophisticated lands in V1.0+.
type BroadcastFilter string

const (
	BroadcastFilterAllActive       BroadcastFilter = "all_active"
	BroadcastFilterExpiredThisWeek BroadcastFilter = "expired_this_week"
)

// BroadcastInput backs UC-041 (POST /api/v1/broadcasts). Owner authors a
// freeform Spanish message; we render it through the `broadcast_freeform`
// template so the WhatsApp side stays inside an approved skeleton.
type BroadcastInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	Filter      BroadcastFilter
	Message     string

	// Confirmed is the DA-41.2 step ("Enviar a 87 socios?"). When false the
	// use case returns a preview without enqueuing.
	Confirmed bool
}

type BroadcastOutput struct {
	Preview     bool
	AudienceN   int
	EnqueuedN   int
	BroadcastID uuid.UUID
}

// MaxAudience is a guardrail against accidental "all members" pushes when
// the filter is broken. ADR-007 §4.6 — Tier 1 numbers cap at 1000
// conversations/24h, so 500 is a safe MVP ceiling.
const MaxAudience = 500

type Broadcast struct {
	Notifications notiRepo.NotificationRepository
	Members       memRepo.MemberRepository
	Gyms          gymRepo.GymRepository
	UoW           sharedDomain.UnitOfWork
	Audit         audit.Recorder
}

func NewBroadcast(
	notifications notiRepo.NotificationRepository,
	members memRepo.MemberRepository,
	gyms gymRepo.GymRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *Broadcast {
	return &Broadcast{
		Notifications: notifications,
		Members:       members,
		Gyms:          gyms,
		UoW:           uow,
		Audit:         recorder,
	}
}

func (uc *Broadcast) Execute(ctx context.Context, in BroadcastInput) (*BroadcastOutput, error) {
	msg := strings.TrimSpace(in.Message)
	if msg == "" {
		return nil, sharedDomain.NewValidationError(notiErrors.ErrBroadcastMessage)
	}
	now := time.Now().UTC()

	out := BroadcastOutput{Preview: !in.Confirmed, BroadcastID: uuid.New()}
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		gym, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return err
		}
		if !gym.IsWhatsAppConnected() {
			return sharedDomain.NewBusinessError(notiErrors.ErrNotConnected, "")
		}

		audience, err := uc.resolveAudience(tx, in.GymID, in.Filter, now)
		if err != nil {
			return err
		}
		if len(audience) == 0 {
			return sharedDomain.NewBusinessError(notiErrors.ErrBroadcastEmpty, "")
		}
		if len(audience) > MaxAudience {
			return sharedDomain.NewBusinessError(notiErrors.ErrBroadcastTooBig, "")
		}
		out.AudienceN = len(audience)
		if !in.Confirmed {
			return nil
		}

		gymName := ""
		if gym.Name != nil {
			gymName = *gym.Name
		}
		for _, m := range audience {
			vars := map[string]string{
				"member_first_name": firstName(m.FullName),
				"gym_name":          gymName,
				"message":           msg,
			}
			n, err := notiDomain.New(
				uuid.New(), in.GymID, m.ID,
				notiDomain.ChannelWhatsApp,
				"broadcast_freeform",
				notiDomain.RecipientMember,
				m.Phone,
				vars,
				now, now,
				nil, // broadcasts intentionally non-idempotent — owner re-sends are real intent
			)
			if err != nil {
				continue
			}
			if _, err := uc.Notifications.Create(tx, n); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			out.EnqueuedN++
		}

		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "broadcasts",
			EntityID:    out.BroadcastID,
			Action:      "broadcast_send",
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"filter":     string(in.Filter),
				"audience_n": out.AudienceN,
				"enqueued_n": out.EnqueuedN,
				"message":    msg,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// audienceMember is a tiny projection — only the fields the broadcast
// renders need.
type audienceMember struct {
	ID       uuid.UUID
	FullName string
	Phone    string
}

func (uc *Broadcast) resolveAudience(tx sharedDomain.Transaction, gymID uuid.UUID, filter BroadcastFilter, now time.Time) ([]audienceMember, error) {
	switch filter {
	case "", BroadcastFilterAllActive:
		return uc.audienceAllActive(tx, gymID, now)
	case BroadcastFilterExpiredThisWeek:
		return uc.audienceExpiredThisWeek(tx, gymID, now)
	default:
		return nil, sharedDomain.NewValidationError(notiErrors.ErrInvalidVariables)
	}
}

func (uc *Broadcast) audienceAllActive(tx sharedDomain.Transaction, gymID uuid.UUID, now time.Time) ([]audienceMember, error) {
	q := memRepo.ListQuery{
		GymID:        gymID,
		StatusFilter: "active",
		Page:         1,
		PageSize:     MaxAudience + 1,
		Today:        now,
	}
	rows, _, err := uc.Members.List(tx, q)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out := make([]audienceMember, 0, len(rows))
	for _, r := range rows {
		if r == nil || r.Member == nil {
			continue
		}
		if strings.TrimSpace(r.Member.Phone) == "" {
			continue
		}
		out = append(out, audienceMember{ID: r.Member.ID, FullName: r.Member.FullName, Phone: r.Member.Phone})
	}
	return out, nil
}

func (uc *Broadcast) audienceExpiredThisWeek(tx sharedDomain.Transaction, gymID uuid.UUID, now time.Time) ([]audienceMember, error) {
	q := memRepo.ListQuery{
		GymID:        gymID,
		StatusFilter: "expiring_soon",
		Page:         1,
		PageSize:     MaxAudience + 1,
		Today:        now,
	}
	rows, _, err := uc.Members.List(tx, q)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out := make([]audienceMember, 0, len(rows))
	for _, r := range rows {
		if r == nil || r.Member == nil {
			continue
		}
		if strings.TrimSpace(r.Member.Phone) == "" {
			continue
		}
		out = append(out, audienceMember{ID: r.Member.ID, FullName: r.Member.FullName, Phone: r.Member.Phone})
	}
	return out, nil
}
