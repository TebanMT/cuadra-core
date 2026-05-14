package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// AssignPinInput backs the partial UC-032 (PIN assignment lives in members BC,
// the actual check-in flow lives in checkins/Sesión 5). When PlainPin is
// empty the use case auto-generates a unique 4-digit PIN.
type AssignPinInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	MemberID    uuid.UUID
	PlainPin    string // optional — if empty we generate one
}

type AssignPinOutput struct {
	MemberID    uuid.UUID
	Pin         string // returned ONCE so the operator can write it on the membership card
	PinDispatch WelcomePinDispatchResult
}

type AssignPin struct {
	Members memRepo.MemberRepository
	// WelcomePin — misma seam que CreateMember. Reasignar/cambiar el PIN
	// dispara una nueva notificación (idempotencia incluye el pin nuevo
	// como sufijo), así el socio recibe el código actualizado vía
	// WhatsApp sin que el operador tenga que copiarlo a mano.
	WelcomePin WelcomePinNotifier
	UoW        sharedDomain.UnitOfWork
	Audit      audit.Recorder
}

func NewAssignPin(members memRepo.MemberRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *AssignPin {
	return &AssignPin{Members: members, UoW: uow, Audit: recorder}
}

// WithWelcomePinNotifier wires the WhatsApp seam used to re-send the PIN
// when the operator regenerates it.
func (uc *AssignPin) WithWelcomePinNotifier(n WelcomePinNotifier) *AssignPin {
	uc.WelcomePin = n
	return uc
}

// errPinExhausted is returned by the auto-generator when 50 attempts at a
// random PIN all collided — only happens at >9000 members per gym, by which
// time we'll have moved to longer PINs.
var errPinExhausted = errors.New("no se pudo generar un PIN único; el gimnasio está saturado de PINs en uso")

func (uc *AssignPin) Execute(ctx context.Context, in AssignPinInput) (*AssignPinOutput, error) {
	now := time.Now().UTC()
	if in.PlainPin != "" {
		if err := memberDomain.ValidatePin(in.PlainPin); err != nil {
			return nil, sharedDomain.NewValidationError(err)
		}
	}

	var out AssignPinOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		m, err := uc.Members.GetByID(tx, in.MemberID)
		if err != nil {
			return err
		}
		if m.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
		}

		pin := in.PlainPin
		if pin == "" {
			generated, gerr := generateUniquePin(tx, uc.Members, in.GymID, &in.MemberID)
			if gerr != nil {
				return sharedDomain.NewBusinessError(gerr, "")
			}
			pin = generated
		} else {
			collides, err := uc.Members.PinHashCollidesInGym(tx, in.GymID, pin, &in.MemberID)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if collides {
				return sharedDomain.NewBusinessError(memErrors.ErrPinAlreadyTaken, "")
			}
		}

		hash, err := auth.HashPassword(pin)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		m.SetPin(hash, pin, now)
		if _, err := uc.Members.Update(tx, m); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// Encolar (re)envío del PIN al socio por WhatsApp. Mismo seam que
		// CreateMember; la idempotencia incluye el pin como sufijo así un
		// PIN nuevo siempre se reenvía.
		notifier := uc.WelcomePin
		if notifier == nil {
			notifier = noopWelcomePinNotifier{}
		}
		res, nerr := notifier.Notify(ctx, tx, WelcomePinNotifyInput{
			GymID: in.GymID, MemberID: m.ID, Pin: pin,
		}, now)
		if nerr != nil {
			return nerr
		}
		out.PinDispatch = res
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "members",
			EntityID:    m.ID,
			Action:      "assign_pin",
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"pin_assigned": true},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = AssignPinOutput{MemberID: m.ID, Pin: pin}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// generateUniquePin tries up to 50 random 4-digit PINs and returns one that
// doesn't collide in the gym. The repo's PinHashCollidesInGym is
// O(N_pins_in_gym) per attempt — fine for gyms up to a few thousand members.
func generateUniquePin(tx sharedDomain.Transaction, repo memRepo.MemberRepository, gymID uuid.UUID, exclude *uuid.UUID) (string, error) {
	const maxAttempts = 50
	for i := 0; i < maxAttempts; i++ {
		pin, err := randomFourDigitPin()
		if err != nil {
			return "", err
		}
		collides, err := repo.PinHashCollidesInGym(tx, gymID, pin, exclude)
		if err != nil {
			return "", err
		}
		if !collides {
			return pin, nil
		}
	}
	return "", errPinExhausted
}

func randomFourDigitPin() (string, error) {
	out := make([]byte, 4)
	max := big.NewInt(10)
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = byte('0' + n.Int64())
	}
	// Avoid weak PINs (0000, 1234, 1111, ...). Also keeps audit-friendliness.
	pin := string(out)
	if isWeakPin(pin) {
		// Try again — the caller's loop retries.
		return fmt.Sprintf("%04d", randomNonWeakInt()), nil
	}
	return pin, nil
}

func isWeakPin(p string) bool {
	if p == "0000" || p == "1234" || p == "1111" || p == "2222" || p == "3333" ||
		p == "4444" || p == "5555" || p == "6666" || p == "7777" || p == "8888" || p == "9999" {
		return true
	}
	return false
}

// randomNonWeakInt returns a random number in [1000, 9999] excluding obvious
// weak codes. Used as a fallback inside randomFourDigitPin.
func randomNonWeakInt() int {
	for {
		n, err := rand.Int(rand.Reader, big.NewInt(9000))
		if err != nil {
			return 5839 // last-ditch deterministic fallback (acceptable: rand.Int rarely fails)
		}
		v := int(n.Int64()) + 1000
		if !isWeakPin(fmt.Sprintf("%04d", v)) {
			return v
		}
	}
}
