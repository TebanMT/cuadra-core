package cashclose

import (
	"time"

	"github.com/google/uuid"
)

var transferNamespace = uuid.MustParse("6239a2f1-39f1-55b6-a628-f01da0e20352")

func DeterministicTransferID(sessionID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(transferNamespace, []byte(sessionID.String()+"|withdrawal"))
}

// CashTransfer is the auditable drawer -> fund event produced by Withdraw.
// It intentionally has no expense category: moving money between locations
// changes neither income nor outflow of the business.
type CashTransfer struct {
	ID            uuid.UUID
	GymID         uuid.UUID
	Version       int
	SessionID     uuid.UUID
	DrawerID      uuid.UUID
	Destination   string
	Amount        float64
	TransferredAt time.Time
	TransferredBy uuid.UUID
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time
}

func NewTransfer(session *CashCloseEvent) *CashTransfer {
	if session == nil || session.Status != StatusWithdrawn || session.WithdrawnCash == nil ||
		session.WithdrawalDestination == nil || session.WithdrawnAt == nil || session.WithdrawnBy == nil {
		return nil
	}
	id := DeterministicTransferID(session.ID)
	return &CashTransfer{
		ID:            id,
		GymID:         session.GymID,
		Version:       1,
		SessionID:     session.ID,
		DrawerID:      session.DrawerID,
		Destination:   *session.WithdrawalDestination,
		Amount:        *session.WithdrawnCash,
		TransferredAt: session.WithdrawnAt.UTC(),
		TransferredBy: *session.WithdrawnBy,
		CreatedAt:     session.WithdrawnAt.UTC(),
		UpdatedAt:     session.WithdrawnAt.UTC(),
	}
}
