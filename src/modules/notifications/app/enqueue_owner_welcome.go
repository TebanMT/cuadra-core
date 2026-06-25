package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// EnqueueOwnerWelcome encola el banner "tu sistema ya está vivo" (owner_welcome)
// al dueño cuando vincula el primer dispositivo del gym (ADR-010 / onboarding).
// Implementa usersApp.OwnerWelcomeNotifier; corre dentro de la tx del caller
// (RedeemInstallerBootstrap) para ser atómico con la regeneración del código
// de acceso + el flag de fire-once del gym.
//
// Idempotencia por gym (`owner_welcome:<gymID>`) como red secundaria: aunque el
// gym flag ya garantiza el disparo único, evita una fila duplicada si dos
// caminos corrieran a la vez.
type EnqueueOwnerWelcome struct {
	Notifications notiRepo.NotificationRepository
	Gyms          gymRepo.GymRepository
	Users         userRepo.UserRepository
}

func NewEnqueueOwnerWelcome(
	notifications notiRepo.NotificationRepository,
	gyms gymRepo.GymRepository,
	users userRepo.UserRepository,
) *EnqueueOwnerWelcome {
	return &EnqueueOwnerWelcome{Notifications: notifications, Gyms: gyms, Users: users}
}

// Notify satisfies usersApp.OwnerWelcomeNotifier.
func (uc *EnqueueOwnerWelcome) Notify(
	ctx context.Context,
	tx sharedDomain.Transaction,
	in usersApp.OwnerWelcomeNotifyInput,
	now time.Time,
) (usersApp.OwnerWelcomeDispatch, error) {
	out := usersApp.OwnerWelcomeDispatch{}

	gym, err := uc.Gyms.GetByID(tx, in.GymID)
	if err != nil {
		return out, sharedDomain.NewUnexpectedError(err)
	}
	user, err := uc.Users.GetByID(tx, in.UserID)
	if err != nil {
		return out, sharedDomain.NewUnexpectedError(err)
	}
	if user.GymID != in.GymID {
		out.SkippedReason = "cross_gym"
		return out, nil
	}
	phone := ""
	if user.Phone != nil {
		phone = strings.TrimSpace(*user.Phone)
	}
	if phone == "" {
		out.SkippedReason = "no_user_phone"
		return out, nil
	}

	gymName := ""
	if gym.Name != nil {
		gymName = *gym.Name
	}
	vars := map[string]string{
		"full_name": user.FullName,
		"gym_name":  gymName,
		"pin":       in.PIN,
	}

	idempKey := fmt.Sprintf("owner_welcome:%s", in.GymID.String())
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
		user.ID,
		notiDomain.ChannelWhatsApp,
		"owner_welcome",
		notiDomain.RecipientUser,
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
