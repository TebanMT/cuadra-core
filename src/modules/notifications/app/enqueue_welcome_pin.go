package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// EnqueueWelcomePin is the notifications-side implementation of
// memApp.WelcomePinNotifier. It writes a single row to notification_queue
// using the caller's transaction so the enqueue stays atomic with the PIN
// assignment in members.
//
// Gating, in order:
//
//  1. gym.IsWhatsAppConnected — no point queuing if the gym hasn't finished
//     UC-037; the dispatcher would fail outbound and the row would just sit
//     in `failed`.
//  2. gym.NotifyMemberPinEnabled — opt-out toggle per gym (default true).
//  3. member is in the same gym (defense in depth against a wiring bug).
//  4. member has a non-empty phone — without a recipient address we can't
//     dispatch and the domain New() constructor would refuse the row.
//
// Returning (result, nil) with Dispatched=false is the explicit "skipped
// silently" signal. Real infra failures (DB write, gym not found) bubble
// up as errors.
type EnqueueWelcomePin struct {
	Notifications notiRepo.NotificationRepository
	Gyms          gymRepo.GymRepository
	Members       memRepo.MemberRepository
}

func NewEnqueueWelcomePin(
	notifications notiRepo.NotificationRepository,
	gyms gymRepo.GymRepository,
	members memRepo.MemberRepository,
) *EnqueueWelcomePin {
	return &EnqueueWelcomePin{Notifications: notifications, Gyms: gyms, Members: members}
}

// Notify satisfies memApp.WelcomePinNotifier.
func (uc *EnqueueWelcomePin) Notify(
	ctx context.Context,
	tx sharedDomain.Transaction,
	in memApp.WelcomePinNotifyInput,
	now time.Time,
) (memApp.WelcomePinDispatchResult, error) {
	out := memApp.WelcomePinDispatchResult{}

	gym, err := uc.Gyms.GetByID(tx, in.GymID)
	if err != nil {
		return out, sharedDomain.NewUnexpectedError(err)
	}
	if !gym.IsWhatsAppConnected() {
		out.SkippedReason = "whatsapp_not_connected"
		return out, nil
	}
	if !gym.NotifyMemberPinEnabled() {
		out.SkippedReason = "disabled_by_gym"
		return out, nil
	}

	member, err := uc.Members.GetByID(tx, in.MemberID)
	if err != nil {
		return out, sharedDomain.NewUnexpectedError(err)
	}
	if member.GymID != in.GymID {
		out.SkippedReason = "cross_gym"
		return out, nil
	}
	phone := strings.TrimSpace(member.Phone)
	if phone == "" {
		out.SkippedReason = "no_member_phone"
		return out, nil
	}

	gymName := ""
	if gym.Name != nil {
		gymName = *gym.Name
	}
	vars := map[string]string{
		"member_first_name": firstName(member.FullName),
		"gym_name":          gymName,
		"pin":               in.Pin,
	}

	// Idempotency: scope by member + pin so that re-generating the PIN
	// produces a new outbound message (the operator wants the socio to
	// receive the NEW code) while accidental double-fires of the same
	// (member, pin) pair collapse to one row.
	idempKey := fmt.Sprintf("welcome_pin:%s:%s", in.MemberID.String(), in.Pin)
	existing, err := uc.Notifications.GetByIdempotencyKey(tx, in.GymID, idempKey)
	if err != nil {
		return out, sharedDomain.NewUnexpectedError(err)
	}
	if existing != nil {
		out.Dispatched = true
		out.RecipientPhone = phone
		return out, nil
	}

	n, err := notiDomain.New(
		uuid.New(),
		in.GymID,
		member.ID,
		notiDomain.ChannelWhatsApp,
		"member_welcome_pin",
		notiDomain.RecipientMember,
		phone,
		vars,
		now, now,
		&idempKey,
	)
	if err != nil {
		return out, sharedDomain.NewValidationError(err)
	}
	if _, err := uc.Notifications.Create(tx, n); err != nil {
		return out, sharedDomain.NewUnexpectedError(err)
	}
	out.Dispatched = true
	out.RecipientPhone = phone
	return out, nil
}
