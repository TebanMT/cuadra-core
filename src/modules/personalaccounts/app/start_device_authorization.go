package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type DeviceAuthorizationCandidate struct {
	ID        string `json:"id"`
	Platform  string `json:"platform"`
	Label     string `json:"label"`
	PublicKey string `json:"public_key"`
}
type DeviceAuthorizationResult struct {
	ID             string                       `json:"id"`
	Candidate      DeviceAuthorizationCandidate `json:"candidate"`
	ComparisonCode string                       `json:"comparison_code"`
	Status         string                       `json:"status"`
	CreatedAt      string                       `json:"created_at"`
	ExpiresAt      string                       `json:"expires_at"`
}

func (DeviceAuthorizationResult) String() string {
	return "device authorization response (candidate redacted)"
}
func (r DeviceAuthorizationResult) GoString() string { return r.String() }

func deviceAuthorizationResult(a *deviceauthorization.Authorization, now time.Time) DeviceAuthorizationResult {
	return DeviceAuthorizationResult{ID: a.ID.String(), Candidate: DeviceAuthorizationCandidate{ID: a.Candidate.ID.String(), Platform: a.Candidate.Platform, Label: a.Candidate.Label, PublicKey: base64.RawURLEncoding.EncodeToString(a.Candidate.PublicKey[:])}, ComparisonCode: a.ComparisonCode, Status: string(a.EffectiveStatus(now)), CreatedAt: accessTimestamp(a.CreatedAt), ExpiresAt: accessTimestamp(a.ExpiresAt)}
}

type StartDeviceAuthorizationInput struct {
	Binding         attempt.Binding
	AuthorizationID uuid.UUID
	OperationID     uuid.UUID
	RequestHash     [32]byte
}

func (StartDeviceAuthorizationInput) String() string {
	return "device authorization request (binding redacted)"
}
func (in StartDeviceAuthorizationInput) GoString() string { return in.String() }

type StartDeviceAuthorizationDependencies struct {
	UoW            shared.UnitOfWork
	Attempts       repository.AuthAttemptRepository
	Authorizations repository.DeviceAuthorizationRepository
	Receipts       repository.AuthReceiptRepository
	Events         repository.SecurityEventRepository
	Now            func() time.Time
}
type StartDeviceAuthorization struct {
	dependencies StartDeviceAuthorizationDependencies
}

func NewStartDeviceAuthorization(d StartDeviceAuthorizationDependencies) (*StartDeviceAuthorization, error) {
	if d.UoW == nil || d.Attempts == nil || d.Authorizations == nil || d.Receipts == nil || d.Events == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &StartDeviceAuthorization{dependencies: d}, nil
}

func readAuthorizationAttempt(ctx context.Context, uow shared.UnitOfWork, attempts repository.AuthAttemptRepository, binding attempt.Binding) (context.Context, *attempt.AuthAttempt, error) {
	if binding.Validate() != nil {
		return ctx, nil, personalErrors.ErrInvalidAuthAttempt
	}
	if err := shared.ValidatePersonalAnonymousScope(ctx); err != nil {
		return ctx, nil, err
	}
	ctx = shared.WithPersonalAttempt(ctx, binding.ID, binding.ContinuationHash)
	q, err := uow.Query(ctx)
	if err != nil {
		return ctx, nil, err
	}
	a, err := attempts.Get(q, binding)
	shared.CloseQuery(q)
	if err != nil {
		return ctx, nil, err
	}
	if a.Purpose != attempt.Access {
		return ctx, nil, personalErrors.ErrAuthAttemptState
	}
	if a.AccountID != nil {
		ctx = shared.WithPersonalAccount(ctx, *a.AccountID)
	}
	return ctx, a, nil
}

func (u *StartDeviceAuthorization) Execute(ctx context.Context, in StartDeviceAuthorizationInput) (DeviceAuthorizationResult, error) {
	if !deviceauthorization.ValidID(in.OperationID) || !deviceauthorization.ValidID(in.AuthorizationID) || in.RequestHash == [32]byte{} {
		return DeviceAuthorizationResult{}, personalErrors.ErrInvalidDeviceAuthorization
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d := u.dependencies
	ctx, before, err := readAuthorizationAttempt(ctx, d.UoW, d.Attempts, in.Binding)
	if err != nil {
		return DeviceAuthorizationResult{}, err
	}
	if before.AccountID == nil {
		return DeviceAuthorizationResult{}, personalErrors.ErrDeviceAuthorizationState
	}
	accountID := *before.AccountID
	var result DeviceAuthorizationResult
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		a, err := d.Attempts.GetForUpdate(tx, in.Binding)
		if err != nil {
			return err
		}
		if a.AccountID == nil || *a.AccountID != accountID || a.Purpose != attempt.Access {
			return personalErrors.ErrDeviceAuthorizationState
		}
		owner := receipt.Owner{AccountID: accountID, AttemptID: a.ID}
		previous, err := d.Receipts.Get(tx, owner, in.OperationID)
		if err == nil {
			if err = previous.Match("device_authorization_start", in.RequestHash); err != nil {
				return err
			}
			if previous.ResultReference != in.AuthorizationID {
				return personalErrors.ErrIdempotencyMismatch
			}
			authorization, err := d.Authorizations.GetForAttempt(tx, accountID, in.Binding)
			if err != nil {
				return err
			}
			if authorization.ID != in.AuthorizationID || authorization.Candidate != a.Candidate {
				return personalErrors.ErrIdempotencyMismatch
			}
			// Return the current terminal/pending state, never recreate a request.
			result = deviceAuthorizationResult(authorization, d.Now())
			return nil
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		code, err := rand.Int(rand.Reader, big.NewInt(1000000))
		if err != nil {
			return personalErrors.ErrInvalidSecurityConfiguration
		}
		authorization, err := deviceauthorization.New(in.AuthorizationID, a, fmt.Sprintf("%06d", code.Int64()), now)
		if err != nil {
			return err
		}
		if err = d.Authorizations.Create(tx, in.Binding, authorization); err != nil {
			return err
		}
		r, err := receipt.New(in.OperationID, owner, "device_authorization_start", in.RequestHash, now)
		if err != nil {
			return err
		}
		r.ResultReference = authorization.ID
		if err = d.Receipts.Create(tx, r); err != nil {
			return err
		}
		if err = d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: accountID, OperationID: in.OperationID, Type: "device_authorization_requested", OccurredAt: now}); err != nil {
			return err
		}
		result = deviceAuthorizationResult(authorization, now)
		return nil
	})
	if err != nil {
		return DeviceAuthorizationResult{}, err
	}
	return result, nil
}
