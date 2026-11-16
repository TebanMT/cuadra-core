package app

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type GetDeviceAuthorizationDecisionResultInput struct {
	AuthorizationID   uuid.UUID
	OperationID       uuid.UUID
	OriginalSessionID uuid.UUID
	OriginalDeviceID  uuid.UUID
	Decision          deviceauthorization.Decision
	RequestHash       [32]byte
}

type DeviceAuthorizationDecisionResultStatus string

const (
	DeviceAuthorizationDecisionCompleted  DeviceAuthorizationDecisionResultStatus = "completed"
	DeviceAuthorizationDecisionNotApplied DeviceAuthorizationDecisionResultStatus = "not_applied"
	DeviceAuthorizationDecisionUnresolved DeviceAuthorizationDecisionResultStatus = "unresolved"
)

// This receipt projection contains no candidate data or credential material.
// A missing receipt is conclusive only after the original session is terminal.
type DeviceAuthorizationDecisionResult struct {
	AccountID         string                                  `json:"account_id"`
	AuthorizationID   string                                  `json:"authorization_id"`
	OperationID       string                                  `json:"operation_id"`
	OriginalSessionID string                                  `json:"original_session_id"`
	OriginalDeviceID  string                                  `json:"original_device_id"`
	Decision          deviceauthorization.Decision            `json:"decision"`
	RequestHash       string                                  `json:"request_hash"`
	Status            DeviceAuthorizationDecisionResultStatus `json:"status"`
	CompletedAt       *string                                 `json:"completed_at"`
	CheckedAt         string                                  `json:"checked_at"`
}

type GetDeviceAuthorizationDecisionResultDependencies struct {
	UoW      shared.UnitOfWork
	Sessions repository.AuthSessionRepository
	Receipts repository.AuthReceiptRepository
	Now      func() time.Time
}

type GetDeviceAuthorizationDecisionResult struct {
	dependencies GetDeviceAuthorizationDecisionResultDependencies
}

func NewGetDeviceAuthorizationDecisionResult(d GetDeviceAuthorizationDecisionResultDependencies) (*GetDeviceAuthorizationDecisionResult, error) {
	if d.UoW == nil || d.Sessions == nil || d.Receipts == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &GetDeviceAuthorizationDecisionResult{dependencies: d}, nil
}

func (u *GetDeviceAuthorizationDecisionResult) Execute(ctx context.Context, actor security.AccessClaims, in GetDeviceAuthorizationDecisionResultInput) (DeviceAuthorizationDecisionResult, error) {
	if !validPersonalActor(actor) {
		return DeviceAuthorizationDecisionResult{}, personalErrors.ErrInvalidAccessToken
	}
	for _, id := range []uuid.UUID{in.AuthorizationID, in.OperationID, in.OriginalSessionID, in.OriginalDeviceID} {
		if !deviceauthorization.ValidID(id) {
			return DeviceAuthorizationDecisionResult{}, personalErrors.ErrInvalidDeviceAuthorization
		}
	}
	if in.RequestHash == [32]byte{} || (in.Decision != deviceauthorization.Approve && in.Decision != deviceauthorization.Deny) {
		return DeviceAuthorizationDecisionResult{}, personalErrors.ErrInvalidDeviceAuthorization
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return DeviceAuthorizationDecisionResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d := u.dependencies
	var result DeviceAuthorizationDecisionResult
	// Command acquires the same account mutex as decision, refresh and
	// revocation. It prevents observing absence ahead of an original commit.
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		if now.IsZero() {
			return personalErrors.ErrInvalidSecurityConfiguration
		}
		result = DeviceAuthorizationDecisionResult{
			AccountID: actor.AccountID.String(), AuthorizationID: in.AuthorizationID.String(), OperationID: in.OperationID.String(),
			OriginalSessionID: in.OriginalSessionID.String(), OriginalDeviceID: in.OriginalDeviceID.String(), Decision: in.Decision,
			RequestHash: hex.EncodeToString(in.RequestHash[:]), CheckedAt: accessTimestamp(now),
		}
		owner := receipt.Owner{AccountID: actor.AccountID}
		previous, err := d.Receipts.Get(tx, owner, in.OperationID)
		if err == nil {
			if previous == nil || previous.Owner != owner || previous.OperationID != in.OperationID {
				return personalErrors.ErrInvalidReceipt
			}
			if err = previous.Match(deviceAuthorizationDecisionPurpose, in.RequestHash); err != nil {
				return err
			}
			if len(previous.ResultSecretHash) != 32 || !deviceauthorization.ValidID(previous.ResultReference) {
				return personalErrors.ErrInvalidReceipt
			}
			original := security.AccessClaims{AccountID: actor.AccountID, SessionID: in.OriginalSessionID, DeviceID: in.OriginalDeviceID}
			digest := authorizationActorDigest(original, in.AuthorizationID, in.OperationID)
			if previous.ResultReference != in.AuthorizationID || subtle.ConstantTimeCompare(previous.ResultSecretHash, digest[:]) != 1 {
				return personalErrors.ErrIdempotencyMismatch
			}
			at := accessTimestamp(previous.CompletedAt)
			result.Status, result.CompletedAt = DeviceAuthorizationDecisionCompleted, &at
			return nil
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		original, err := d.Sessions.GetSession(tx, actor.AccountID, in.OriginalSessionID)
		if errors.Is(err, personalErrors.ErrAuthObjectNotFound) {
			return personalErrors.ErrDeviceAuthorizationNotFound
		}
		if err != nil {
			return err
		}
		if original == nil || original.Validate() != nil {
			return personalErrors.ErrInvalidSession
		}
		if original.AccountID != actor.AccountID || original.ID != in.OriginalSessionID || original.DeviceID != in.OriginalDeviceID {
			return personalErrors.ErrDeviceAuthorizationNotFound
		}
		result.Status = DeviceAuthorizationDecisionUnresolved
		// Device/proof state alone cannot rule out later execution: a renewed
		// proof may still authorize the operation while its session survives.
		if original.RevokedAt != nil {
			result.Status = DeviceAuthorizationDecisionNotApplied
			return nil
		}
		// The application clock is presentation only. PostgreSQL uses its own
		// clock to admit the original request and refresh, so expiry must use
		// that same clock under this mutex before it can exclude a later effect.
		expired, err := d.Sessions.SessionExpired(tx, actor.AccountID, in.OriginalSessionID)
		if errors.Is(err, personalErrors.ErrAuthObjectNotFound) {
			return personalErrors.ErrDeviceAuthorizationNotFound
		}
		if err != nil {
			return err
		}
		if expired {
			result.Status = DeviceAuthorizationDecisionNotApplied
		}
		return nil
	})
	if err != nil {
		return DeviceAuthorizationDecisionResult{}, err
	}
	return result, nil
}
