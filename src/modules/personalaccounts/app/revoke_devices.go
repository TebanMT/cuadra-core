package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

const devicesRevocationPurpose = "devices_revoke"

type RevokeDevicesInput struct {
	OperationID uuid.UUID
	RequestHash [32]byte
	ProofToken  string
	Target      device.RevocationTarget
}

func (RevokeDevicesInput) String() string   { return "personal device revocation [proof redacted]" }
func (RevokeDevicesInput) GoString() string { return "personal device revocation [proof redacted]" }

type RevokeDevicesDependencies struct {
	UoW      shared.UnitOfWork
	Sessions repository.AuthSessionRepository
	Devices  repository.DevicesRepository
	Proofs   repository.DeviceProofRepository
	Receipts repository.AuthReceiptRepository
	Events   repository.SecurityEventRepository
	Now      func() time.Time
}
type RevokeDevices struct{ dependencies RevokeDevicesDependencies }

func NewRevokeDevices(d RevokeDevicesDependencies) (*RevokeDevices, error) {
	if d.UoW == nil || d.Sessions == nil || d.Devices == nil || d.Proofs == nil || d.Receipts == nil || d.Events == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &RevokeDevices{dependencies: d}, nil
}

func (u *RevokeDevices) Execute(ctx context.Context, actor security.AccessClaims, in RevokeDevicesInput) (Acknowledgement, error) {
	if !validPersonalActor(actor) {
		return Acknowledgement{}, personalErrors.ErrInvalidAccessToken
	}
	if in.OperationID == uuid.Nil || in.OperationID.Variant() != uuid.RFC4122 || in.OperationID.Version() < 1 || in.OperationID.Version() > 8 || in.RequestHash == [32]byte{} || in.Target.Validate() != nil {
		return Acknowledgement{}, personalErrors.ErrInvalidDevice
	}
	if !validRefreshSecret(in.ProofToken) {
		return Acknowledgement{}, personalErrors.ErrInvalidProof
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return Acknowledgement{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d := u.dependencies
	var ack Acknowledgement
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
			return err
		}
		owner := receipt.Owner{AccountID: actor.AccountID}
		previous, err := d.Receipts.Get(tx, owner, in.OperationID)
		if err == nil {
			if err = previous.Match(devicesRevocationPurpose, in.RequestHash); err != nil {
				return err
			}
			if previous.ResultReference != actor.SessionID {
				return personalErrors.ErrInvalidProof
			}
			ack = recoveryAcknowledgement(previous)
			return nil
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		now := d.Now().UTC()
		if now.IsZero() || now.Year() < 1 || now.Year() > 9999 {
			return personalErrors.ErrInvalidDevice
		}
		if err = d.Proofs.Consume(tx, actor, sha256.Sum256([]byte(in.ProofToken)), security.RevokeDevices, in.RequestHash, in.OperationID, now); err != nil {
			return err
		}
		if err = d.Devices.Revoke(tx, actor, in.Target, now); err != nil {
			return err
		}
		r, err := receipt.New(in.OperationID, owner, devicesRevocationPurpose, in.RequestHash, now)
		if err != nil {
			return err
		}
		// This result contains no secret. Bind its durable acknowledgement to
		// the original session; never use it to bypass live authorization.
		r.ResultReference = actor.SessionID
		if err = d.Receipts.Create(tx, r); err != nil {
			return err
		}
		if err = d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID, OperationID: in.OperationID, Type: "devices_revoked", OccurredAt: now}); err != nil {
			return err
		}
		ack = recoveryAcknowledgement(r)
		return nil
	})
	if err != nil {
		return Acknowledgement{}, err
	}
	return ack, nil
}
