package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/recovery"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type SessionTokens struct {
	AccountID        string `json:"account_id"`
	DeviceID         string `json:"device_id"`
	SessionID        string `json:"session_id"`
	AccessToken      string `json:"access_token"`
	AccessExpiresAt  string `json:"access_expires_at"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresAt string `json:"refresh_expires_at"`
	Generation       string `json:"generation"`
	RevocationTicket string `json:"revocation_ticket"`
}

func (SessionTokens) String() string     { return "personal session (credentials redacted)" }
func (s SessionTokens) GoString() string { return s.String() }

type RecoveryKit struct {
	ID         string `json:"id"`
	Secret     string `json:"secret"`
	Generation string `json:"generation"`
}

func (RecoveryKit) String() string     { return "recovery kit (secret redacted)" }
func (k RecoveryKit) GoString() string { return k.String() }

type AuthorizedResult struct {
	Status      string        `json:"status"`
	Session     SessionTokens `json:"session"`
	RecoveryKit *RecoveryKit  `json:"recovery_kit"`
}
type AccessResult struct {
	Attempt               *attempt.AuthAttempt
	Authorized            *AuthorizedResult
	AuthorizationRequired bool
	StateOverride         DeviceAuthorizationAttemptState
}
type FinalizeAccessDependencies struct {
	UoW             shared.UnitOfWork
	Attempts        repository.AuthAttemptRepository
	Identity        repository.IdentityAccessRepository
	Sessions        repository.AuthSessionRepository
	Recovery        repository.RecoveryKeyRepository
	Receipts        repository.AuthReceiptRepository
	Events          repository.SecurityEventRepository
	Tokens          security.AccessTokens
	Proofs          security.DeviceProofs
	RecoverySecrets security.RecoveryKeys
	Envelopes       security.Envelopes
	Now             func() time.Time
}
type FinalizeAccessInput struct {
	Binding         attempt.Binding
	OperationID     uuid.UUID
	RequestHash     [32]byte
	DeviceSignature string
}
type FinalizeAccess struct{ dependencies FinalizeAccessDependencies }

func NewFinalizeAccess(d FinalizeAccessDependencies) (*FinalizeAccess, error) {
	if d.UoW == nil || d.Attempts == nil || d.Identity == nil || d.Sessions == nil || d.Recovery == nil || d.Receipts == nil || d.Events == nil || d.Tokens == nil || d.Proofs == nil || d.RecoverySecrets == nil || d.Envelopes == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &FinalizeAccess{dependencies: d}, nil
}
func randomAccessSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", personalErrors.ErrInvalidSecurityConfiguration
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func accessTimestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Execute never calls Verify. A prior transaction must have persisted a direct
// successful OTP check. This transaction takes phone -> account -> challenge,
// then commits device/session/kit/consumption/audit/encrypted result together.
func (u *FinalizeAccess) Execute(ctx context.Context, in FinalizeAccessInput) (AccessResult, error) {
	d := u.dependencies
	if in.Binding.Validate() != nil || in.OperationID == uuid.Nil || in.RequestHash == [32]byte{} {
		return AccessResult{}, personalErrors.ErrInvalidAuthAttempt
	}
	ctx = shared.WithPersonalAttempt(ctx, in.Binding.ID, in.Binding.ContinuationHash)
	q, err := d.UoW.Query(ctx)
	if err != nil {
		return AccessResult{}, err
	}
	a, err := d.Attempts.Get(q, in.Binding)
	shared.CloseQuery(q)
	if err != nil {
		return AccessResult{}, err
	}
	if a.Purpose != attempt.Access {
		return AccessResult{}, personalErrors.ErrAuthAttemptState
	}
	if a.State == attempt.Consumed {
		return u.replay(ctx, in)
	}
	if a.State != attempt.Verified {
		return AccessResult{Attempt: a}, nil
	}
	if !approvedCheckMatches(a, in) {
		return AccessResult{}, personalErrors.ErrAuthAttemptOperation
	}
	var result AccessResult
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		bound, err := d.Identity.ResolveVerifiedAccess(tx, in.Binding.ID)
		if err != nil {
			return err
		}
		a, err := d.Attempts.GetForUpdate(tx, in.Binding)
		if err != nil {
			return err
		}
		if !approvedCheckMatches(a, in) {
			return personalErrors.ErrAuthAttemptOperation
		}
		revision := a.Revision
		r, err := d.Receipts.Get(tx, receipt.Owner{AttemptID: in.Binding.ID}, in.OperationID)
		if err != nil {
			return err
		}
		if err = r.Match("auth_check", in.RequestHash); err != nil {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		if err = a.BindAccount(bound.AccountID, now); err != nil {
			return err
		}
		method := "resume_device"
		if bound.Created {
			newDevice, err := device.NewAuthorizedDevice(bound.AccountID, a.Candidate, device.Signup, now)
			if err != nil {
				return err
			}
			if err = d.Sessions.CreateDevice(tx, bound.AccountID, newDevice); err != nil {
				return err
			}
			method = "signup"
		} else {
			existing, deviceErr := d.Sessions.GetDevice(tx, bound.AccountID, a.Candidate.ID)
			if deviceErr != nil && !errors.Is(deviceErr, personalErrors.ErrAuthObjectNotFound) {
				return deviceErr
			}
			resumed := deviceErr == nil && existing.ActiveAt(now) && existing.PublicKey == a.Candidate.PublicKey && in.DeviceSignature != ""
			if resumed {
				proof := security.ResumeProof{AttemptID: a.ID, DeviceID: a.Candidate.ID, Nonce: a.Nonce[:], ExpiresAt: a.ExpiresAt}
				if err = d.Proofs.VerifyResume(existing.PublicKey[:], in.DeviceSignature, proof, now); err != nil {
					return err
				}
			}
			if !resumed {
				if a.Revision > revision {
					if err = d.Attempts.Save(tx, in.Binding, a, revision); err != nil {
						return err
					}
				}
				result = AccessResult{Attempt: a, AuthorizationRequired: true}
				return nil
			}
		}
		refresh, err := randomAccessSecret()
		if err != nil {
			return err
		}
		ticket, err := randomAccessSecret()
		if err != nil {
			return err
		}
		s, err := session.NewSession(bound.AccountID, a.Candidate.ID, uuid.New(), sha256.Sum256([]byte(refresh)), uuid.New(), sha256.Sum256([]byte(ticket)), now)
		if err != nil {
			return err
		}
		if err = d.Sessions.CreateSession(tx, bound.AccountID, s); err != nil {
			return err
		}
		access, claims, err := d.Tokens.Issue(bound.AccountID, s.ID, s.DeviceID, now)
		if err != nil {
			return err
		}
		payload := &AuthorizedResult{Status: "authorized", Session: SessionTokens{AccountID: bound.AccountID.String(), DeviceID: s.DeviceID.String(), SessionID: s.ID.String(), AccessToken: access, AccessExpiresAt: accessTimestamp(claims.ExpiresAt), RefreshToken: refresh, RefreshExpiresAt: accessTimestamp(s.ExpiresAt), Generation: strconv.FormatInt(s.Generation, 10), RevocationTicket: ticket}}
		if bound.Created {
			secret, err := randomAccessSecret()
			if err != nil {
				return err
			}
			verifier, err := d.RecoverySecrets.CreateVerifier(secret)
			if err != nil {
				return err
			}
			key, err := recovery.NewKey(bound.AccountID, uuid.New(), 1, verifier, now)
			if err != nil {
				return err
			}
			if err = d.Recovery.Create(tx, bound.AccountID, key); err != nil {
				return err
			}
			payload.RecoveryKit = &RecoveryKit{ID: key.ID.String(), Secret: secret, Generation: "1"}
		}
		if err = a.Consume(in.OperationID, now); err != nil {
			return err
		}
		if err = d.Attempts.Save(tx, in.Binding, a, revision); err != nil {
			return err
		}
		if err = d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: bound.AccountID, ActorDeviceID: a.Candidate.ID, OperationID: in.OperationID, Type: method, OccurredAt: now}); err != nil {
			return err
		}
		plain, err := json.Marshal(payload)
		if err != nil {
			return personalErrors.ErrInvalidReceipt
		}
		envelope, err := d.Envelopes.SealReceipt(r.Context(), plain)
		if err != nil {
			return err
		}
		if err = r.SetResponse(envelope, r.CompletedAt.Add(10*time.Minute), s.ID); err != nil {
			return err
		}
		if err = d.Receipts.SaveResponse(tx, r); err != nil {
			return err
		}
		result = AccessResult{Authorized: payload}
		return nil
	})
	if errors.Is(err, personalErrors.ErrAccessIdentityUnavailable) {
		// A concurrent identical finalizer may have consumed the attempt while
		// this caller waited for its phone lock. Only the exact receipt can replay.
		return u.replay(ctx, in)
	}
	return result, err
}

func (u *FinalizeAccess) replay(ctx context.Context, in FinalizeAccessInput) (AccessResult, error) {
	d := u.dependencies
	q, err := d.UoW.Query(ctx)
	if err != nil {
		return AccessResult{}, err
	}
	a, err := d.Attempts.Get(q, in.Binding)
	if err != nil {
		shared.CloseQuery(q)
		return AccessResult{}, err
	}
	if a.State != attempt.Consumed || a.AccountID == nil || a.ResultReference == nil || *a.ResultReference != in.OperationID {
		shared.CloseQuery(q)
		return AccessResult{}, personalErrors.ErrAccessIdentityUnavailable
	}
	shared.CloseQuery(q)
	accountID := *a.AccountID
	var result AccessResult
	err = d.UoW.Command(shared.WithPersonalAccount(ctx, accountID), func(tx shared.Transaction) error {
		r, err := d.Receipts.Get(tx, receipt.Owner{AttemptID: in.Binding.ID}, in.OperationID)
		if err != nil {
			return err
		}
		if err = r.Match("auth_check", in.RequestHash); err != nil {
			return err
		}
		if !approvedCheckMatches(a, in) {
			return personalErrors.ErrAuthAttemptOperation
		}
		envelope, err := r.ReadResponse(d.Now())
		if err != nil {
			return err
		}
		plain, err := d.Envelopes.OpenReceipt(r.Context(), envelope)
		if err != nil {
			return err
		}
		var payload AuthorizedResult
		decoder := json.NewDecoder(bytes.NewReader(plain))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&payload); err != nil || payload.Status != "authorized" || payload.Session.AccountID != accountID.String() || payload.Session.DeviceID != a.Candidate.ID.String() {
			return personalErrors.ErrInvalidReceipt
		}
		sid, err := uuid.Parse(payload.Session.SessionID)
		if err != nil {
			return personalErrors.ErrInvalidReceipt
		}
		active, err := d.Sessions.SessionActive(tx, accountID, sid, a.Candidate.ID)
		if err != nil {
			return err
		}
		if !active {
			return personalErrors.ErrSessionRevoked
		}
		result = AccessResult{Authorized: &payload}
		return nil
	})
	return result, err
}

// A receipt from an earlier rejected check belongs to the same attempt but
// did not authorize it. Only the exact durable check operation that produced
// Verified may consume the attempt and receive its authorized result.
func approvedCheckMatches(a *attempt.AuthAttempt, in FinalizeAccessInput) bool {
	return a != nil && a.ProviderOperation != nil && a.ProviderOperation.Kind == attempt.Check &&
		a.ProviderOperation.ID == in.OperationID && subtle.ConstantTimeCompare(a.ProviderOperation.RequestHash[:], in.RequestHash[:]) == 1
}
