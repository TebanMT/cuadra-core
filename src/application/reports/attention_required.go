// UC-034 — Atención inmediata.
//
// One read endpoint that surfaces every actionable thing a operator should
// look at right now. The owner UI groups them into sections; we return them
// already split so the controller stays dumb.
package reports

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Thresholds are spec-driven (UC-034 / DA-34.1) and kept as named constants
// so they're greppable from product discussions.
const (
	expiringSoonDays   = 7
	recoverableMaxDays = 60
	staleContactDays   = 7  // "sin contacto reciente"
	inactiveAbsentDays = 21 // DA-34.1 "sin checkin 21+ días"
)

type AttentionRequiredInput struct {
	GymID uuid.UUID
}

type AttentionRequiredOutput struct {
	GeneratedAt time.Time `json:"generated_at"`

	ExpiringSoon        []MemberExpiringRow  `json:"expiring_soon"`
	RecoverableExpired  []MemberExpiredRow   `json:"recoverable_expired"`
	InactiveInvoluntary []MemberInactiveRow  `json:"inactive_involuntary"`
	LowStock            []ProductLowStockRow `json:"low_stock"`
	PendingBalances     []PendingBalanceRow  `json:"pending_balances"`
	BirthdaysToday      []MemberBirthdayRow  `json:"birthdays_today"`
}

type AttentionRequired struct {
	Reader Reader
	UoW    sharedDomain.UnitOfWork
	// Gyms (opcional) → "hoy" en el día LOCAL del gym (ver localToday).
	Gyms gymRepo.GymRepository
}

func NewAttentionRequired(reader Reader, uow sharedDomain.UnitOfWork) *AttentionRequired {
	return &AttentionRequired{Reader: reader, UoW: uow}
}

// WithGyms cablea el repo de gyms para anclar el "hoy" de la lista de
// atención al día calendario del gym en SU zona horaria.
func (uc *AttentionRequired) WithGyms(g gymRepo.GymRepository) *AttentionRequired {
	uc.Gyms = g
	return uc
}

func (uc *AttentionRequired) Execute(ctx context.Context, in AttentionRequiredInput) (*AttentionRequiredOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := time.Now().UTC()
	// Día LOCAL del gym — con el día UTC, los por-vencer/vencidos se
	// adelantaban 6 horas cada noche.
	today := localToday(tx, uc.Gyms, in.GymID, now)

	expiring, err := uc.Reader.ListExpiringSoon(tx, in.GymID, today, expiringSoonDays)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	expired, err := uc.Reader.ListExpiredRecoverable(tx, in.GymID, today, recoverableMaxDays, staleContactDays)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	inactive, err := uc.Reader.ListInactiveInvoluntary(tx, in.GymID, today, inactiveAbsentDays)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	stock, err := uc.Reader.ListLowStock(tx, in.GymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	bal, err := uc.Reader.ListPendingBalances(tx, in.GymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	bdays, err := uc.Reader.ListBirthdaysOn(tx, in.GymID, today)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return &AttentionRequiredOutput{
		GeneratedAt:         now,
		ExpiringSoon:        expiring,
		RecoverableExpired:  expired,
		InactiveInvoluntary: inactive,
		LowStock:            stock,
		PendingBalances:     bal,
		BirthdaysToday:      bdays,
	}, nil
}
