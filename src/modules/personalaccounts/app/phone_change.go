package app

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/phonechange"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type PhoneChangeDependencies struct {
	Phone     StartAuthDependencies
	Identity  repository.PhoneChangeRepository
	Sessions  repository.AuthSessionRepository
	Proofs    repository.DeviceProofRepository
	Events    repository.SecurityEventRepository
	Tokens    security.AccessTokens
	Envelopes security.Envelopes
	MaxChecks int
}

func validatePhoneChangeDependencies(d PhoneChangeDependencies) (shared.PersonalAuthPreparedUnitOfWork, error) {
	if err := validatePhoneDependencies(d.Phone); err != nil {
		return nil, err
	}
	u, ok := d.Phone.UoW.(shared.PersonalAuthPreparedUnitOfWork)
	if !ok || d.Identity == nil || d.Sessions == nil || d.Proofs == nil || d.Events == nil || d.Tokens == nil || d.Envelopes == nil || d.MaxChecks <= 0 || d.MaxChecks > math.MaxInt32 {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return u, nil
}
func phoneChangeReceipt(d PhoneChangeDependencies, tx shared.Transaction, actor security.AccessClaims, op phoneOperation, purpose string) (*receipt.Receipt, error) {
	r, err := d.Phone.Receipts.Get(tx, receipt.Owner{AccountID: actor.AccountID, AttemptID: op.Binding.ID}, op.OperationID)
	if err != nil {
		return nil, err
	}
	if err = r.Match(purpose, op.RequestHash); err != nil {
		return nil, err
	}
	digest := phonechange.ActorDigest(actor, op.Binding.ID, op.OperationID)
	if subtle.ConstantTimeCompare(r.ResultSecretHash, digest[:]) != 1 {
		return nil, personalErrors.ErrInvalidPhoneChange
	}
	return r, nil
}
func newPhoneChangeReceipt(actor security.AccessClaims, op phoneOperation, purpose string, now time.Time) (*receipt.Receipt, error) {
	r, err := receipt.New(op.OperationID, receipt.Owner{AccountID: actor.AccountID, AttemptID: op.Binding.ID}, purpose, op.RequestHash, now)
	if err != nil {
		return nil, err
	}
	digest := phonechange.ActorDigest(actor, op.Binding.ID, op.OperationID)
	r.ResultSecretHash = append([]byte(nil), digest[:]...)
	r.ResultReference = op.Binding.ID
	if purpose == phonechange.StartPurpose {
		r.ResultReference = actor.SessionID
	}
	return r, nil
}
func phoneChangeActor(d PhoneChangeDependencies, tx shared.Transaction, actor security.AccessClaims, binding attempt.Binding, lock bool) (*attempt.AuthAttempt, error) {
	if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
		return nil, err
	}
	var a *attempt.AuthAttempt
	var err error
	if lock {
		a, err = d.Phone.Attempts.GetForUpdate(tx, binding)
	} else {
		a, err = d.Phone.Attempts.Get(tx, binding)
	}
	if err != nil {
		return nil, err
	}
	if err = d.Identity.CheckInitiator(tx, actor, a); err != nil {
		return nil, err
	}
	return a, nil
}
func persistPhoneChangeOutcome(ctx context.Context, d PhoneChangeDependencies, actor security.AccessClaims, binding attempt.Binding, token attempt.OperationToken, result verification.Result) (*attempt.AuthAttempt, error) {
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var current *attempt.AuthAttempt
	err := d.Phone.UoW.Command(persist, func(tx shared.Transaction) error {
		a, err := phoneChangeActor(d, tx, actor, binding, true)
		if err != nil {
			return err
		}
		current = a
		revision := a.Revision
		now := d.Phone.Now()
		if !now.Before(a.ExpiresAt) && (a.State == attempt.Pending || a.State == attempt.Uncertain || a.State == attempt.Verified) {
			if err = a.Expire(now); err != nil {
				return err
			}
			return d.Phone.Attempts.Save(tx, binding, a, revision)
		}
		if err = a.ApplyOutcome(token, result, now); err != nil {
			if errors.Is(err, personalErrors.ErrAuthAttemptOperation) || errors.Is(err, personalErrors.ErrAuthAttemptState) || errors.Is(err, personalErrors.ErrAuthAttemptExpired) {
				return nil
			}
			return err
		}
		return d.Phone.Attempts.Save(tx, binding, a, revision)
	})
	return current, err
}
func phoneChangePendingError(a *attempt.AuthAttempt) error {
	if a == nil {
		return personalErrors.ErrInvalidPhoneChange
	}
	switch a.State {
	case attempt.Uncertain:
		return personalErrors.ErrAuthAttemptUncertain
	case attempt.Expired:
		return personalErrors.ErrAuthAttemptExpired
	case attempt.Pending:
		return personalErrors.ErrVerificationRejected
	default:
		return personalErrors.ErrAuthAttemptState
	}
}

type phoneChangeResponse struct {
	SessionID uuid.UUID     `json:"actor_session_id"`
	DeviceID  uuid.UUID     `json:"actor_device_id"`
	AttemptID uuid.UUID     `json:"attempt_id"`
	Tokens    SessionTokens `json:"session"`
}

func (phoneChangeResponse) String() string     { return "phone change response (credentials redacted)" }
func (r phoneChangeResponse) GoString() string { return r.String() }
func readPhoneChangeResponse(d PhoneChangeDependencies, tx shared.Transaction, actor security.AccessClaims, a *attempt.AuthAttempt, r *receipt.Receipt) (SessionTokens, error) {
	if a.State != attempt.Consumed || a.ResultReference == nil || *a.ResultReference != r.OperationID || r.ResultReference != actor.SessionID || a.ConsumedAt == nil {
		return SessionTokens{}, personalErrors.ErrInvalidPhoneChange
	}
	return readPhoneChangeTokens(d.Sessions, d.Envelopes, d.Phone.Now, tx, actor, a.ID, r)
}

func readPhoneChangeTokens(sessions repository.AuthSessionRepository, envelopes security.Envelopes, now func() time.Time, tx shared.Transaction, actor security.AccessClaims, attemptID uuid.UUID, r *receipt.Receipt) (SessionTokens, error) {
	envelope, err := r.ReadResponse(now())
	if err != nil {
		return SessionTokens{}, err
	}
	plain, err := envelopes.OpenReceipt(r.Context(), envelope)
	if err != nil {
		return SessionTokens{}, err
	}
	defer clear(plain)
	if len(plain) > 16384 {
		return SessionTokens{}, personalErrors.ErrInvalidEnvelope
	}
	var stored phoneChangeResponse
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&stored) != nil || decoder.Decode(new(any)) != io.EOF || stored.SessionID != actor.SessionID || stored.DeviceID != actor.DeviceID || stored.AttemptID != attemptID {
		return SessionTokens{}, personalErrors.ErrInvalidEnvelope
	}
	result := stored.Tokens
	generation, err := strconv.ParseInt(result.Generation, 10, 64)
	if err != nil || generation < 2 || result.AccountID != actor.AccountID.String() || result.SessionID != actor.SessionID.String() || result.DeviceID != actor.DeviceID.String() || !validRefreshSecret(result.RefreshToken) || !validRefreshSecret(result.RevocationTicket) || len(result.AccessToken) == 0 {
		return SessionTokens{}, personalErrors.ErrInvalidEnvelope
	}
	s, err := sessions.GetSession(tx, actor.AccountID, actor.SessionID)
	if err != nil {
		return SessionTokens{}, err
	}
	if s.DeviceID != actor.DeviceID || s.Generation < generation {
		return SessionTokens{}, personalErrors.ErrInvalidEnvelope
	}
	// A later refresh may already own a newer generation. This exact receipt
	// remains an historical response; it never rotates back to its credentials.
	return result, nil
}
