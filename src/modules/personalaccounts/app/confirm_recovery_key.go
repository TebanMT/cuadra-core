package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

const recoveryConfirmationPurpose = "recovery_key_confirm"

type Acknowledgement struct {
	OperationID string `json:"operation_id"`
	Status      string `json:"status"`
	CompletedAt string `json:"completed_at"`
}
type ConfirmRecoveryKeyInput struct {
	OperationID   uuid.UUID
	RequestHash   [32]byte
	RecoveryKeyID uuid.UUID
	Saved         bool
}
type ConfirmRecoveryKeyDependencies struct {
	UoW       shared.UnitOfWork
	Sessions  repository.AuthSessionRepository
	Recovery  repository.RecoveryKeyRepository
	Receipts  repository.AuthReceiptRepository
	Events    repository.SecurityEventRepository
	Envelopes security.Envelopes
	Now       func() time.Time
}
type ConfirmRecoveryKey struct {
	dependencies ConfirmRecoveryKeyDependencies
}

func NewConfirmRecoveryKey(d ConfirmRecoveryKeyDependencies) (*ConfirmRecoveryKey, error) {
	if d.UoW == nil || d.Sessions == nil || d.Recovery == nil || d.Receipts == nil || d.Events == nil || d.Envelopes == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &ConfirmRecoveryKey{dependencies: d}, nil
}

func (u *ConfirmRecoveryKey) Execute(ctx context.Context, actor security.AccessClaims, in ConfirmRecoveryKeyInput) (Acknowledgement, error) {
	if !validPersonalActor(actor) {
		return Acknowledgement{}, personalErrors.ErrInvalidAccessToken
	}
	if in.OperationID == uuid.Nil || in.RecoveryKeyID == uuid.Nil || in.RequestHash == [32]byte{} || !in.Saved {
		return Acknowledgement{}, personalErrors.ErrInvalidRecoveryConfirmation
	}
	d := u.dependencies
	var ack Acknowledgement
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return Acknowledgement{}, err
	}
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		// Personal UoW holds the account stream mutex before validating actor,
		// reading a receipt or changing the recovery key.
		if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
			return err
		}
		owner := receipt.Owner{AccountID: actor.AccountID}
		previous, err := d.Receipts.Get(tx, owner, in.OperationID)
		if err == nil {
			if err = previous.Match(recoveryConfirmationPurpose, in.RequestHash); err != nil {
				return err
			}
			if previous.ResultReference != in.RecoveryKeyID {
				return personalErrors.ErrIdempotencyMismatch
			}
			// An acknowledgement has no secret. Its durable receipt reconstructs
			// the same result after ciphertext expiry or replacement of the kit.
			ack = recoveryAcknowledgement(previous)
			return nil
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		key, err := d.Recovery.Get(tx, actor.AccountID, in.RecoveryKeyID)
		if err != nil {
			return err
		}
		if key.AccountID != actor.AccountID || !key.Live() {
			return personalErrors.ErrRecoveryKeyState
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		if err = key.ConfirmSaved(now); err != nil {
			return err
		}
		if err = d.Recovery.SaveState(tx, actor.AccountID, key); err != nil {
			return err
		}
		r, err := receipt.New(in.OperationID, owner, recoveryConfirmationPurpose, in.RequestHash, now)
		if err != nil {
			return err
		}
		ack = recoveryAcknowledgement(r)
		plain, err := json.Marshal(ack)
		if err != nil {
			return personalErrors.ErrInvalidReceipt
		}
		envelope, err := d.Envelopes.SealReceipt(r.Context(), plain)
		if err != nil {
			return err
		}
		if err = r.SetResponse(envelope, now.Add(10*time.Minute), key.ID); err != nil {
			return err
		}
		if err = d.Receipts.Create(tx, r); err != nil {
			return err
		}
		return d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID, OperationID: in.OperationID, Type: recoveryConfirmationPurpose, OccurredAt: now})
	})
	if err != nil {
		return Acknowledgement{}, err
	}
	return ack, nil
}

func recoveryAcknowledgement(r *receipt.Receipt) Acknowledgement {
	return Acknowledgement{OperationID: r.OperationID.String(), Status: "completed", CompletedAt: accessTimestamp(r.CompletedAt)}
}
