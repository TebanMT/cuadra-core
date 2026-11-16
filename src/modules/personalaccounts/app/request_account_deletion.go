package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deletion"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type RequestAccountDeletionInput struct {
	OperationID   uuid.UUID
	RequestHash   [32]byte
	Confirmation  string
	PolicyVersion string
	ResultSecret  string
	ProofToken    string
}

func (RequestAccountDeletionInput) String() string {
	return "account deletion request (proof and result secret redacted)"
}
func (r RequestAccountDeletionInput) GoString() string { return r.String() }

type RequestAccountDeletionDependencies struct {
	UoW      shared.UnitOfWork
	Sessions repository.AuthSessionRepository
	Deletion repository.DeletionRepository
	Proofs   repository.DeviceProofRepository
	Receipts repository.AuthReceiptRepository
	Events   repository.SecurityEventRepository
	Policy   *deletion.Policy
	Now      func() time.Time
}
type RequestAccountDeletion struct {
	dependencies RequestAccountDeletionDependencies
}

func NewRequestAccountDeletion(d RequestAccountDeletionDependencies) (*RequestAccountDeletion, error) {
	if d.UoW == nil || d.Sessions == nil || d.Deletion == nil || d.Proofs == nil || d.Receipts == nil || d.Events == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	if d.Policy != nil {
		if err := d.Policy.Validate(); err != nil {
			return nil, err
		}
		copy := *d.Policy
		d.Policy = &copy
	}
	return &RequestAccountDeletion{d}, nil
}

func (u *RequestAccountDeletion) Execute(ctx context.Context, actor security.AccessClaims, in RequestAccountDeletionInput) (Acknowledgement, error) {
	if !validPersonalActor(actor) {
		return Acknowledgement{}, personalErrors.ErrInvalidAccessToken
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return Acknowledgement{}, err
	}
	d := u.dependencies
	if err = d.Policy.Validate(); err != nil {
		return Acknowledgement{}, err
	}
	if in.Confirmation != deletion.Confirmation || in.PolicyVersion != d.Policy.Version || in.RequestHash == [32]byte{} {
		return Acknowledgement{}, personalErrors.ErrInvalidDeletion
	}
	resultHash, err := deletion.ResultHash(in.OperationID, in.ResultSecret)
	if err != nil {
		return Acknowledgement{}, err
	}
	if !validRefreshSecret(in.ProofToken) {
		return Acknowledgement{}, personalErrors.ErrInvalidProof
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var ack Acknowledgement
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
			return err
		}
		owner := receipt.Owner{AccountID: actor.AccountID}
		r, err := d.Receipts.Get(tx, owner, in.OperationID)
		if err == nil {
			if err = r.Match(deletion.Purpose, in.RequestHash); err != nil {
				return err
			}
			if r.ResultReference != actor.SessionID || subtle.ConstantTimeCompare(r.ResultSecretHash, resultHash[:]) != 1 {
				return personalErrors.ErrIdempotencyMismatch
			}
			// No exception for the session withdrawn by the original commit.
			// The separate result capability is the lost-response path.
			result, err := d.Deletion.GetResult(tx, actor.AccountID, in.OperationID, resultHash)
			if err != nil {
				return err
			}
			ack = deletionAcknowledgement(result)
			return nil
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		request := deletion.Request{OperationID: in.OperationID, RequestHash: in.RequestHash, ProofHash: sha256.Sum256([]byte(in.ProofToken)), Policy: *d.Policy, RequestedAt: now}
		if err = request.Validate(); err != nil {
			return err
		}
		if err = d.Proofs.Consume(tx, actor, request.ProofHash, security.DeleteAccount, in.RequestHash, in.OperationID, now); err != nil {
			return err
		}
		if err = d.Deletion.Request(tx, actor, request); err != nil {
			return err
		}
		r, err = receipt.New(in.OperationID, owner, deletion.Purpose, in.RequestHash, now)
		if err != nil {
			return err
		}
		r.ResultReference = actor.SessionID
		r.ResultSecretHash = append([]byte(nil), resultHash[:]...)
		if err = d.Receipts.Create(tx, r); err != nil {
			return err
		}
		if err = d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID, OperationID: in.OperationID, Type: "account_deletion_requested", OccurredAt: now}); err != nil {
			return err
		}
		ack = deletionAcknowledgement(deletion.Result{OperationID: in.OperationID, CompletedAt: now})
		return nil
	})
	if err != nil {
		return Acknowledgement{}, err
	}
	return ack, nil
}

func deletionAcknowledgement(r deletion.Result) Acknowledgement {
	return Acknowledgement{OperationID: r.OperationID.String(), Status: "deletion_pending", CompletedAt: accessTimestamp(r.CompletedAt)}
}
