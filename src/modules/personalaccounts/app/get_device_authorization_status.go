package app

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// Empty means no authorization-specific override. These are the only wire
// states that this reader projects; stored challenge states remain unchanged.
type DeviceAuthorizationAttemptState string

const (
	AuthorizationPending   DeviceAuthorizationAttemptState = "authorization_pending"
	AuthorizationDenied    DeviceAuthorizationAttemptState = "denied"
	AuthorizationExpired   DeviceAuthorizationAttemptState = "expired"
	AuthorizationCancelled DeviceAuthorizationAttemptState = "cancelled"
)

type DeviceAuthorizationStatusResult struct {
	Attempt       *attempt.AuthAttempt
	StateOverride DeviceAuthorizationAttemptState
	Authorized    *AuthorizedResult
}

func (DeviceAuthorizationStatusResult) String() string {
	return "device authorization status (credentials and attempt redacted)"
}
func (r DeviceAuthorizationStatusResult) GoString() string { return r.String() }

type GetDeviceAuthorizationStatusDependencies struct {
	UoW            shared.UnitOfWork
	Attempts       repository.AuthAttemptRepository
	Authorizations repository.DeviceAuthorizationRepository
	Sessions       repository.AuthSessionRepository
	Receipts       repository.AuthReceiptRepository
	Envelopes      security.Envelopes
	Now            func() time.Time
}
type GetDeviceAuthorizationStatus struct {
	dependencies GetDeviceAuthorizationStatusDependencies
}

func NewGetDeviceAuthorizationStatus(d GetDeviceAuthorizationStatusDependencies) (*GetDeviceAuthorizationStatus, error) {
	if d.UoW == nil || d.Attempts == nil || d.Authorizations == nil || d.Sessions == nil || d.Receipts == nil || d.Envelopes == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &GetDeviceAuthorizationStatus{dependencies: d}, nil
}

// The account transaction supplies a mutex and rechecks current authority. It
// performs no writes, provider calls, proof consumption or session creation.
func (u *GetDeviceAuthorizationStatus) Execute(ctx context.Context, binding attempt.Binding) (DeviceAuthorizationStatusResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d := u.dependencies
	ctx, before, err := readAuthorizationAttempt(ctx, d.UoW, d.Attempts, binding)
	if err != nil {
		return DeviceAuthorizationStatusResult{}, err
	}
	if before.AccountID == nil {
		return DeviceAuthorizationStatusResult{Attempt: before}, nil
	}
	accountID := *before.AccountID
	var result DeviceAuthorizationStatusResult
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		a, err := d.Attempts.GetForUpdate(tx, binding)
		if err != nil {
			return err
		}
		if a.AccountID == nil || *a.AccountID != accountID || a.Purpose != attempt.Access {
			return personalErrors.ErrDeviceAuthorizationState
		}
		authorization, err := d.Authorizations.GetForAttempt(tx, accountID, binding)
		if errors.Is(err, personalErrors.ErrDeviceAuthorizationNotFound) && a.State != attempt.Consumed {
			result = DeviceAuthorizationStatusResult{Attempt: a}
			return nil
		}
		if err != nil {
			return err
		}
		if authorization.Candidate != a.Candidate || authorization.AttemptID != a.ID || authorization.AccountID != accountID {
			return personalErrors.ErrDeviceAuthorizationState
		}
		result.Attempt = a
		if a.State == attempt.Consumed {
			if authorization.Status != deviceauthorization.Approved || authorization.DecidedAt == nil || authorization.ApprovedByDeviceID == nil || a.ConsumedAt == nil || !a.ConsumedAt.Equal(*authorization.DecidedAt) || a.ResultReference == nil {
				return personalErrors.ErrDeviceAuthorizationState
			}
			payload, err := readAuthorizedCandidateResult(d, tx, a, authorization)
			if err != nil {
				return err
			}
			result.Authorized = &payload
			return nil
		}
		switch authorization.Status {
		case deviceauthorization.Denied:
			if a.State != attempt.Cancelled {
				return personalErrors.ErrDeviceAuthorizationState
			}
			result.StateOverride = AuthorizationDenied
		case deviceauthorization.Cancelled:
			result.StateOverride = AuthorizationCancelled
		case deviceauthorization.Expired:
			result.StateOverride = AuthorizationExpired
		case deviceauthorization.Pending:
			if a.State == attempt.Cancelled {
				result.StateOverride = AuthorizationCancelled
			} else if a.State == attempt.Expired || !d.Now().Before(a.ExpiresAt) || authorization.EffectiveStatus(d.Now()) == deviceauthorization.Expired {
				result.StateOverride = AuthorizationExpired
			} else if a.State == attempt.Verified {
				result.StateOverride = AuthorizationPending
			} else {
				return personalErrors.ErrDeviceAuthorizationState
			}
		default:
			return personalErrors.ErrDeviceAuthorizationState
		}
		return nil
	})
	if err != nil {
		return DeviceAuthorizationStatusResult{}, err
	}
	return result, nil
}

func readAuthorizedCandidateResult(d GetDeviceAuthorizationStatusDependencies, tx shared.Transaction, a *attempt.AuthAttempt, authorization *deviceauthorization.Authorization) (AuthorizedResult, error) {
	owner := receipt.Owner{AccountID: *a.AccountID, AttemptID: a.ID}
	r, err := d.Receipts.Get(tx, owner, *a.ResultReference)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if r.Purpose != deviceAuthorizationResultPurpose || r.ResultReference != authorization.ID || !r.CompletedAt.Equal(*a.ConsumedAt) {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	envelope, err := r.ReadResponse(d.Now())
	if err != nil {
		return AuthorizedResult{}, err
	}
	plain, err := d.Envelopes.OpenReceipt(r.Context(), envelope)
	if err != nil {
		return AuthorizedResult{}, err
	}
	defer clear(plain)
	if len(plain) > 16384 {
		return AuthorizedResult{}, personalErrors.ErrInvalidEnvelope
	}
	var saved deviceAuthorizationResponse
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&saved); err != nil || decoder.Decode(new(any)) != io.EOF {
		return AuthorizedResult{}, personalErrors.ErrInvalidEnvelope
	}
	if saved.AuthorizationID != authorization.ID || saved.AttemptID != a.ID || saved.Candidate != deviceAuthorizationResult(authorization, d.Now()).Candidate || saved.ApproverDeviceID != *authorization.ApprovedByDeviceID || !deviceauthorization.ValidID(saved.ApproverSessionID) || !deviceauthorization.ValidID(saved.DecisionOperationID) || saved.Authorized.Status != "authorized" || saved.Authorized.RecoveryKit != nil {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	if err = r.Match(deviceAuthorizationResultPurpose, saved.DecisionRequestHash); err != nil {
		return AuthorizedResult{}, err
	}
	decision, err := d.Receipts.Get(tx, receipt.Owner{AccountID: *a.AccountID}, saved.DecisionOperationID)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if err = decision.Match(deviceAuthorizationDecisionPurpose, saved.DecisionRequestHash); err != nil {
		return AuthorizedResult{}, err
	}
	approver := security.AccessClaims{AccountID: *a.AccountID, SessionID: saved.ApproverSessionID, DeviceID: saved.ApproverDeviceID}
	digest := authorizationActorDigest(approver, authorization.ID, saved.DecisionOperationID)
	if decision.ResultReference != authorization.ID || !decision.CompletedAt.Equal(r.CompletedAt) || subtle.ConstantTimeCompare(decision.ResultSecretHash, digest[:]) != 1 {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	tokens := saved.Authorized.Session
	sid, err := uuid.Parse(tokens.SessionID)
	if err != nil || !deviceauthorization.ValidID(sid) || sid.String() != tokens.SessionID || tokens.AccountID != a.AccountID.String() || tokens.DeviceID != a.Candidate.ID.String() {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	active, err := d.Sessions.SessionActive(tx, *a.AccountID, sid, a.Candidate.ID)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if !active {
		return AuthorizedResult{}, personalErrors.ErrSessionRevoked
	}
	createdDevice, err := d.Sessions.GetDevice(tx, *a.AccountID, a.Candidate.ID)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if createdDevice.PublicKey != a.Candidate.PublicKey || createdDevice.Platform != a.Candidate.Platform || createdDevice.Label != a.Candidate.Label || createdDevice.AuthorizedByMethod != "device" || !createdDevice.CreatedAt.Equal(*a.ConsumedAt) {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	createdSession, err := d.Sessions.GetSession(tx, *a.AccountID, sid)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if createdSession.DeviceID != a.Candidate.ID || !createdSession.CreatedAt.Equal(*a.ConsumedAt) {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	return saved.Authorized, nil
}
