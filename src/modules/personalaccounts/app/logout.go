package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type LogoutInput struct {
	OperationID      uuid.UUID
	RequestHash      [32]byte
	RevocationTicket string
}

func (LogoutInput) String() string      { return "personal logout input (ticket redacted)" }
func (in LogoutInput) GoString() string { return in.String() }

type LogoutDependencies struct {
	UoW      shared.UnitOfWork
	Logout   repository.LogoutRepository
	Sessions repository.AuthSessionRepository
	Receipts repository.AuthReceiptRepository
	Events   repository.SecurityEventRepository
	Now      func() time.Time
}
type Logout struct{ dependencies LogoutDependencies }

func NewLogout(d LogoutDependencies) (*Logout, error) {
	if d.UoW == nil || d.Logout == nil || d.Sessions == nil || d.Receipts == nil || d.Events == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &Logout{dependencies: d}, nil
}

func (u *Logout) Execute(ctx context.Context, in LogoutInput) (Acknowledgement, error) {
	if err := shared.ValidatePersonalAnonymousScope(ctx); err != nil {
		return Acknowledgement{}, err
	}
	if in.OperationID == uuid.Nil || in.RequestHash == [32]byte{} || !validRefreshSecret(in.RevocationTicket) {
		return Acknowledgement{}, personalErrors.ErrInvalidLogout
	}
	d := u.dependencies
	hash := sha256.Sum256([]byte(in.RevocationTicket))
	accountID, err := d.Logout.Resolve(ctx, hash)
	if err != nil {
		return Acknowledgement{}, err
	}
	ctx, err = shared.WithAuthenticatedPersonalAccount(ctx, accountID)
	if err != nil {
		return Acknowledgement{}, err
	}
	var ack Acknowledgement
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		s, err := d.Logout.GetSessionForTicket(tx, accountID, hash)
		if err != nil {
			return err
		}
		// A revocation ticket intentionally needs no active session and grants
		// no read/refresh authority. This also works after account withdrawal.
		owner := receipt.Owner{AccountID: accountID}
		previous, err := d.Receipts.Get(tx, owner, in.OperationID)
		if err == nil {
			if err = previous.Match("logout", in.RequestHash); err != nil {
				return err
			}
			if previous.ResultReference != s.ID {
				return personalErrors.ErrIdempotencyMismatch
			}
			ack = logoutAcknowledgement(previous)
			return nil
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		if err = d.Sessions.RevokeSession(tx, accountID, s.ID, now); err != nil {
			return err
		}
		r, err := receipt.New(in.OperationID, owner, "logout", in.RequestHash, now)
		if err != nil {
			return err
		}
		r.ResultReference = s.ID
		if err = d.Receipts.Create(tx, r); err != nil {
			return err
		}
		if err = d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: accountID, ActorDeviceID: s.DeviceID, OperationID: in.OperationID, Type: "logout", OccurredAt: now}); err != nil {
			return err
		}
		ack = logoutAcknowledgement(r)
		return nil
	})
	if err != nil {
		return Acknowledgement{}, err
	}
	return ack, nil
}
func logoutAcknowledgement(r *receipt.Receipt) Acknowledgement {
	return Acknowledgement{OperationID: r.OperationID.String(), Status: "completed", CompletedAt: accessTimestamp(r.CompletedAt)}
}
