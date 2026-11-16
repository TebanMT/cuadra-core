package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

const deviceAuthorizationDecisionPurpose = "device_authorization_decision"
const deviceAuthorizationResultPurpose = "device_authorization_result"

type DecideDeviceAuthorizationInput struct {
	AuthorizationID uuid.UUID
	OperationID     uuid.UUID
	RequestHash     [32]byte
	Decision        deviceauthorization.Decision
	ProofToken      string
}

func (DecideDeviceAuthorizationInput) String() string {
	return "device authorization decision (proof redacted)"
}
func (in DecideDeviceAuthorizationInput) GoString() string { return in.String() }

type DecideDeviceAuthorizationDependencies struct {
	UoW            shared.UnitOfWork
	Attempts       repository.AuthAttemptRepository
	Authorizations repository.DeviceAuthorizationRepository
	Sessions       repository.AuthSessionRepository
	Proofs         repository.DeviceProofRepository
	Receipts       repository.AuthReceiptRepository
	Events         repository.SecurityEventRepository
	Tokens         security.AccessTokens
	Envelopes      security.Envelopes
	Now            func() time.Time
}
type DecideDeviceAuthorization struct {
	dependencies DecideDeviceAuthorizationDependencies
}

func NewDecideDeviceAuthorization(d DecideDeviceAuthorizationDependencies) (*DecideDeviceAuthorization, error) {
	if d.UoW == nil || d.Attempts == nil || d.Authorizations == nil || d.Sessions == nil || d.Proofs == nil || d.Receipts == nil || d.Events == nil || d.Tokens == nil || d.Envelopes == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &DecideDeviceAuthorization{dependencies: d}, nil
}

// Fixed-width UUIDs with an explicit domain prevent ambiguous concatenations.
// This is durable actor binding, not a bearer credential or a result lookup.
func authorizationActorDigest(actor security.AccessClaims, authorizationID, operationID uuid.UUID) [32]byte {
	input := []byte("mancuerna.device_authorization_decision.actor.v1\x00")
	for _, id := range []uuid.UUID{actor.AccountID, actor.SessionID, actor.DeviceID, authorizationID, operationID} {
		input = append(input, id[:]...)
	}
	return sha256.Sum256(input)
}

type deviceAuthorizationResponse struct {
	AuthorizationID     uuid.UUID                    `json:"authorization_id"`
	AttemptID           uuid.UUID                    `json:"attempt_id"`
	Candidate           DeviceAuthorizationCandidate `json:"candidate"`
	ApproverSessionID   uuid.UUID                    `json:"approver_session_id"`
	ApproverDeviceID    uuid.UUID                    `json:"approver_device_id"`
	DecisionOperationID uuid.UUID                    `json:"decision_operation_id"`
	DecisionRequestHash [32]byte                     `json:"decision_request_hash"`
	Authorized          AuthorizedResult             `json:"authorized"`
}

func (deviceAuthorizationResponse) String() string {
	return "device authorization result (credentials redacted)"
}
func (r deviceAuthorizationResponse) GoString() string { return r.String() }

func (u *DecideDeviceAuthorization) Execute(ctx context.Context, actor security.AccessClaims, in DecideDeviceAuthorizationInput) (Acknowledgement, error) {
	if !validPersonalActor(actor) {
		return Acknowledgement{}, personalErrors.ErrInvalidAccessToken
	}
	if !deviceauthorization.ValidID(in.AuthorizationID) || !deviceauthorization.ValidID(in.OperationID) || in.RequestHash == [32]byte{} || (in.Decision != deviceauthorization.Approve && in.Decision != deviceauthorization.Deny) {
		return Acknowledgement{}, personalErrors.ErrInvalidDeviceAuthorization
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
			if err = previous.Match(deviceAuthorizationDecisionPurpose, in.RequestHash); err != nil {
				return err
			}
			if previous.ResultReference != in.AuthorizationID {
				return personalErrors.ErrIdempotencyMismatch
			}
			digest := authorizationActorDigest(actor, in.AuthorizationID, in.OperationID)
			if subtle.ConstantTimeCompare(previous.ResultSecretHash, digest[:]) != 1 {
				return personalErrors.ErrInvalidProof
			}
			a, _, err := d.Authorizations.GetForActor(tx, actor, in.AuthorizationID)
			if err != nil {
				return err
			}
			if (in.Decision == deviceauthorization.Approve && a.Status != deviceauthorization.Approved) || (in.Decision == deviceauthorization.Deny && a.Status != deviceauthorization.Denied) {
				return personalErrors.ErrDeviceAuthorizationState
			}
			ack = recoveryAcknowledgement(previous)
			return nil
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		authorization, binding, err := d.Authorizations.GetForActor(tx, actor, in.AuthorizationID)
		if err != nil {
			return err
		}
		a, err := d.Attempts.GetForUpdate(tx, binding)
		if err != nil {
			return err
		}
		if a.AccountID == nil || *a.AccountID != actor.AccountID || a.ID != authorization.AttemptID || a.Candidate != authorization.Candidate || a.Purpose != attempt.Access || a.State != attempt.Verified {
			return personalErrors.ErrDeviceAuthorizationState
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		if !now.Before(a.ExpiresAt) {
			return personalErrors.ErrDeviceAuthorizationExpired
		}
		if err = authorization.Decide(actor.DeviceID, in.Decision, now); err != nil {
			return err
		}
		if err = d.Proofs.Consume(tx, actor, sha256.Sum256([]byte(in.ProofToken)), security.AuthorizeDevice, in.RequestHash, in.OperationID, now); err != nil {
			return err
		}
		revision := a.Revision
		if in.Decision == deviceauthorization.Approve {
			payload, err := createAuthorizationSession(d, tx, a, now)
			if err != nil {
				return err
			}
			resultReceipt, err := receipt.New(uuid.New(), receipt.Owner{AccountID: actor.AccountID, AttemptID: a.ID}, deviceAuthorizationResultPurpose, in.RequestHash, now)
			if err != nil {
				return err
			}
			response := deviceAuthorizationResponse{AuthorizationID: authorization.ID, AttemptID: a.ID, Candidate: deviceAuthorizationResult(authorization, now).Candidate, ApproverSessionID: actor.SessionID, ApproverDeviceID: actor.DeviceID, DecisionOperationID: in.OperationID, DecisionRequestHash: in.RequestHash, Authorized: payload}
			plain, err := json.Marshal(response)
			if err != nil {
				return personalErrors.ErrInvalidReceipt
			}
			defer clear(plain)
			envelope, err := d.Envelopes.SealReceipt(resultReceipt.Context(), plain)
			if err != nil {
				return err
			}
			if err = resultReceipt.SetResponse(envelope, now.Add(10*time.Minute), authorization.ID); err != nil {
				return err
			}
			if err = d.Receipts.Create(tx, resultReceipt); err != nil {
				return err
			}
			if err = a.Consume(resultReceipt.OperationID, now); err != nil {
				return err
			}
		} else {
			if err = a.Cancel(now); err != nil {
				return err
			}
		}
		if err = d.Attempts.Save(tx, binding, a, revision); err != nil {
			return err
		}
		if err = d.Authorizations.SaveDecision(tx, actor, authorization); err != nil {
			return err
		}
		r, err := receipt.New(in.OperationID, owner, deviceAuthorizationDecisionPurpose, in.RequestHash, now)
		if err != nil {
			return err
		}
		r.ResultReference = authorization.ID
		digest := authorizationActorDigest(actor, authorization.ID, in.OperationID)
		r.ResultSecretHash = append([]byte(nil), digest[:]...)
		if err = d.Receipts.Create(tx, r); err != nil {
			return err
		}
		eventType := "device_authorization_approved"
		if in.Decision == deviceauthorization.Deny {
			eventType = "device_authorization_denied"
		}
		if err = d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID, OperationID: in.OperationID, Type: eventType, OccurredAt: now}); err != nil {
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

func createAuthorizationSession(d DecideDeviceAuthorizationDependencies, tx shared.Transaction, a *attempt.AuthAttempt, now time.Time) (AuthorizedResult, error) {
	newDevice, err := device.NewAuthorizedDevice(*a.AccountID, a.Candidate, device.Device, now)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if err = d.Sessions.CreateDevice(tx, *a.AccountID, newDevice); err != nil {
		return AuthorizedResult{}, err
	}
	refresh, err := randomAccessSecret()
	if err != nil {
		return AuthorizedResult{}, err
	}
	ticket, err := randomAccessSecret()
	if err != nil {
		return AuthorizedResult{}, err
	}
	s, err := session.NewSession(*a.AccountID, a.Candidate.ID, uuid.New(), sha256.Sum256([]byte(refresh)), uuid.New(), sha256.Sum256([]byte(ticket)), now)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if err = d.Sessions.CreateSession(tx, *a.AccountID, s); err != nil {
		return AuthorizedResult{}, err
	}
	access, claims, err := d.Tokens.Issue(*a.AccountID, s.ID, s.DeviceID, now)
	if err != nil {
		return AuthorizedResult{}, err
	}
	return AuthorizedResult{Status: "authorized", Session: SessionTokens{AccountID: a.AccountID.String(), DeviceID: s.DeviceID.String(), SessionID: s.ID.String(), AccessToken: access, AccessExpiresAt: accessTimestamp(claims.ExpiresAt), RefreshToken: refresh, RefreshExpiresAt: accessTimestamp(s.ExpiresAt), Generation: strconv.FormatInt(s.Generation, 10), RevocationTicket: ticket}}, nil
}
