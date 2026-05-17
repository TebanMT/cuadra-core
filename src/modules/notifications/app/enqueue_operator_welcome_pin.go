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

// EnqueueOperatorWelcomePIN writes the operator_welcome_pin notification.
//
// Gating, in order:
//
//  1. gym.IsWhatsAppConnected — no point queuing if outbound would fail.
//  2. user is an operator of the same gym (defense in depth against a
//     wiring bug from CreateOperator).
//  3. user has a non-empty phone — sin recipient address no podemos
//     despachar, y NewOperator ya garantiza esto en el happy path; lo
//     dejamos por si un legacy operator entra al flujo de rotación.
//
// Idempotency: clave por user+pin para que regenerar el PIN dispare un
// mensaje nuevo (el dueño quiere que el operador reciba el código nuevo)
// pero un doble-clic accidental colapse a una fila sola.
type EnqueueOperatorWelcomePIN struct {
	Notifications notiRepo.NotificationRepository
	Gyms          gymRepo.GymRepository
	Users         userRepo.UserRepository
}

func NewEnqueueOperatorWelcomePIN(
	notifications notiRepo.NotificationRepository,
	gyms gymRepo.GymRepository,
	users userRepo.UserRepository,
) *EnqueueOperatorWelcomePIN {
	return &EnqueueOperatorWelcomePIN{
		Notifications: notifications,
		Gyms:          gyms,
		Users:         users,
	}
}

// Notify satisfies usersApp.OperatorWelcomePINNotifier.
func (uc *EnqueueOperatorWelcomePIN) Notify(
	ctx context.Context,
	tx sharedDomain.Transaction,
	in usersApp.OperatorWelcomePINNotifyInput,
	now time.Time,
) (usersApp.OperatorWelcomePINDispatch, error) {
	out := usersApp.OperatorWelcomePINDispatch{}

	gym, err := uc.Gyms.GetByID(tx, in.GymID)
	if err != nil {
		return out, sharedDomain.NewUnexpectedError(err)
	}
	if !gym.IsWhatsAppConnected() {
		out.SkippedReason = "whatsapp_not_connected"
		return out, nil
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

	idempKey := fmt.Sprintf("operator_welcome_pin:%s:%s", in.UserID.String(), in.PIN)
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
		"operator_welcome_pin",
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
