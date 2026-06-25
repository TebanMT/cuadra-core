package app

import (
	"context"
	"crypto/rand"
	"math/big"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// MemberNumberDigitsStore is the cross-BC seam members uses to read and
// (monotonically) grow the per-gym member_number length (ADR-010 §2.1). The
// gyms BC implements it over the gym's KioskSettings JSON; main.go wires it.
// A nil store falls back to defaultMemberNumberDigitsStore (fixed default
// digits, no bump) — used by tests that don't exercise the gym config.
type MemberNumberDigitsStore interface {
	Digits(tx sharedDomain.Transaction, gymID uuid.UUID) (int, error)
	Grow(tx sharedDomain.Transaction, gymID uuid.UUID, digits int) error
}

type defaultMemberNumberDigitsStore struct{}

func (defaultMemberNumberDigitsStore) Digits(sharedDomain.Transaction, uuid.UUID) (int, error) {
	return memberDomain.DefaultMemberNumberDigits, nil
}
func (defaultMemberNumberDigitsStore) Grow(sharedDomain.Transaction, uuid.UUID, int) error {
	return nil
}

// AssignMemberNumberInput backs UC-032 (the member-number assignment lives in
// the members BC; the actual check-in flow lives in checkins). When Number is
// 0 the use case auto-generates a unique number for the gym (ADR-010 §2.2).
type AssignMemberNumberInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	MemberID    uuid.UUID
	Number      int // optional — 0 = generate
}

type AssignMemberNumberOutput struct {
	MemberID     uuid.UUID
	MemberNumber int // returned so the operator can write it on the credential
	Dispatch     WelcomeDispatchResult
}

type AssignMemberNumber struct {
	Members memRepo.MemberRepository
	// Digits resolves/grows the per-gym member_number length. Defaults to a
	// fixed-default store; main.go swaps in the gyms-backed impl.
	Digits MemberNumberDigitsStore
	// Welcome — misma seam que CreateMember. Reasignar/cambiar el número
	// dispara una nueva notificación (la idempotencia incluye el número
	// nuevo como sufijo), así el socio recibe el número actualizado vía
	// WhatsApp sin que el operador tenga que copiarlo a mano.
	Welcome WelcomeNotifier
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
}

func NewAssignMemberNumber(members memRepo.MemberRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *AssignMemberNumber {
	return &AssignMemberNumber{Members: members, Digits: defaultMemberNumberDigitsStore{}, UoW: uow, Audit: recorder}
}

// WithWelcomeNotifier wires the WhatsApp seam used to (re)send the number
// when the operator assigns or regenerates it.
func (uc *AssignMemberNumber) WithWelcomeNotifier(n WelcomeNotifier) *AssignMemberNumber {
	uc.Welcome = n
	return uc
}

// WithDigitsStore wires the gyms-backed config seam (length + bump).
func (uc *AssignMemberNumber) WithDigitsStore(s MemberNumberDigitsStore) *AssignMemberNumber {
	if s != nil {
		uc.Digits = s
	}
	return uc
}

func (uc *AssignMemberNumber) Execute(ctx context.Context, in AssignMemberNumberInput) (*AssignMemberNumberOutput, error) {
	now := time.Now().UTC()
	if in.Number != 0 {
		if err := memberDomain.ValidateMemberNumber(in.Number); err != nil {
			return nil, sharedDomain.NewValidationError(err)
		}
	}

	var out AssignMemberNumberOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		m, err := uc.Members.GetByID(tx, in.MemberID)
		if err != nil {
			return err
		}
		if m.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
		}

		number := in.Number
		if number == 0 {
			generated, gerr := generateUniqueMemberNumber(tx, uc.Members, uc.digitsStore(), in.GymID)
			if gerr != nil {
				return sharedDomain.NewBusinessError(gerr, "")
			}
			number = generated
		} else {
			taken, err := uc.Members.MemberNumberExistsInGym(tx, in.GymID, number, &in.MemberID)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if taken {
				return sharedDomain.NewBusinessError(memErrors.ErrMemberNumberAlreadyTaken, "")
			}
		}

		m.SetMemberNumber(number, now)
		if _, err := uc.Members.Update(tx, m); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// Encolar (re)envío del número al socio por WhatsApp. Mismo seam que
		// CreateMember; la idempotencia incluye el número como sufijo así un
		// número nuevo siempre se reenvía.
		notifier := uc.Welcome
		if notifier == nil {
			notifier = noopWelcomeNotifier{}
		}
		res, nerr := notifier.Notify(ctx, tx, WelcomeNotifyInput{
			GymID: in.GymID, MemberID: m.ID, Number: number,
		}, now)
		if nerr != nil {
			return nerr
		}

		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "members",
			EntityID:    m.ID,
			Action:      "assign_member_number",
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"member_number": number},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = AssignMemberNumberOutput{MemberID: m.ID, MemberNumber: number, Dispatch: res}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (uc *AssignMemberNumber) digitsStore() MemberNumberDigitsStore {
	if uc.Digits == nil {
		return defaultMemberNumberDigitsStore{}
	}
	return uc.Digits
}

const (
	memberNumberMaxRandomAttempts = 50
	// memberNumberBumpRatio: cuando los socios usados consumen >= este % del
	// espacio del dígito actual, se sube un dígito (ADR-010 §2.2 — ~50%).
	// Mantener el espacio ≤50% lleno vuelve las colisiones random
	// improbables (~2 intentos esperados) y deja el gap-fill casi vestigial.
	memberNumberBumpRatio = 0.5
)

// generateUniqueMemberNumber implements the offline-optimistic asignador
// (ADR-010 §2.2): bump preventivo del dígito al ~50% de consumo → random en
// el rango → gap-fill al colisionar. La unicidad real la garantiza el índice
// `uq_members_gym_number` + la reconciliación al sync (projectMember); aquí
// sólo evitamos colisiones contra el estado local conocido.
func generateUniqueMemberNumber(tx sharedDomain.Transaction, repo memRepo.MemberRepository, digits MemberNumberDigitsStore, gymID uuid.UUID) (int, error) {
	d, err := digits.Digits(tx, gymID)
	if err != nil {
		return 0, err
	}
	if d < memberDomain.DefaultMemberNumberDigits {
		d = memberDomain.DefaultMemberNumberDigits
	}

	used, err := repo.ListUsedMemberNumbers(tx, gymID)
	if err != nil {
		return 0, err
	}

	// Bump preventivo: sube D (y persiste) mientras los usados consuman
	// >= bumpRatio del espacio del dígito actual. En la práctica corre a lo
	// más una vez por llamada (tras subir, el espacio nuevo queda <<50%).
	for {
		lo, hi := memberNumberRange(d)
		if float64(len(used)) < memberNumberBumpRatio*float64(hi-lo+1) {
			break
		}
		d++
		if err := digits.Grow(tx, gymID, d); err != nil {
			return 0, err
		}
	}

	lo, hi := memberNumberRange(d)
	usedSet := make(map[int]struct{}, len(used))
	for _, n := range used {
		usedSet[n] = struct{}{}
	}

	// 1) Random en el rango del dígito actual.
	span := int64(hi - lo + 1)
	for i := 0; i < memberNumberMaxRandomAttempts; i++ {
		r, err := rand.Int(rand.Reader, big.NewInt(span))
		if err != nil {
			return 0, err
		}
		n := lo + int(r.Int64())
		if _, taken := usedSet[n]; !taken {
			return n, nil
		}
	}
	// 2) Gap-fill: el hueco más bajo del rango. Sólo se alcanza si el espacio
	// está denso pese al bump (escenario marginal).
	for n := lo; n <= hi; n++ {
		if _, taken := usedSet[n]; !taken {
			return n, nil
		}
	}
	return 0, memErrors.ErrMemberNumberExhausted
}

// memberNumberRange returns [10^(D-1), 10^D - 1] — el rango de D dígitos.
func memberNumberRange(digits int) (int, int) {
	lo := 1
	for i := 1; i < digits; i++ {
		lo *= 10
	}
	hi := lo*10 - 1
	return lo, hi
}
