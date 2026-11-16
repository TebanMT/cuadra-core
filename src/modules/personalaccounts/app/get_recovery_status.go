package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
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

type GetRecoveryStatusDependencies struct {
	UoW             shared.UnitOfWork
	Attempts        repository.AuthAttemptRepository
	Recovery        repository.RecoveryKeyRepository
	Sessions        repository.AuthSessionRepository
	Receipts        repository.AuthReceiptRepository
	RecoverySecrets security.RecoveryKeys
	Envelopes       security.Envelopes
	Now             func() time.Time
}
type GetRecoveryStatus struct{ dependencies GetRecoveryStatusDependencies }

func NewGetRecoveryStatus(d GetRecoveryStatusDependencies) (*GetRecoveryStatus, error) {
	if d.UoW == nil || d.Attempts == nil || d.Recovery == nil || d.Sessions == nil || d.Receipts == nil || d.RecoverySecrets == nil || d.Envelopes == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &GetRecoveryStatus{dependencies: d}, nil
}
func recoveryReaderDependencies(d CompleteRecoveryDependencies) GetRecoveryStatusDependencies {
	return GetRecoveryStatusDependencies{UoW: d.UoW, Attempts: d.Attempts, Recovery: d.Recovery, Sessions: d.Sessions, Receipts: d.Receipts, RecoverySecrets: d.RecoverySecrets, Envelopes: d.Envelopes, Now: d.Now}
}

// This reader only returns an already committed recovery effect. In particular
// verified recovery does not imply authority and never triggers finalization.
func (u *GetRecoveryStatus) Execute(ctx context.Context, binding attempt.Binding) (AccessResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d := u.dependencies
	a, err := readRecoveryAttempt(ctx, d.UoW, d.Attempts, binding)
	if err != nil {
		return AccessResult{}, err
	}
	if a.State != attempt.Consumed {
		return AccessResult{Attempt: a, AuthorizationRequired: a.State == attempt.Verified}, nil
	}
	if a.AccountID == nil {
		return AccessResult{}, personalErrors.ErrRecoveryKeyState
	}
	scope := shared.WithPersonalAccount(shared.WithPersonalAttempt(ctx, binding.ID, binding.ContinuationHash), *a.AccountID)
	var result AuthorizedResult
	err = d.UoW.Command(scope, func(tx shared.Transaction) error {
		current, err := d.Attempts.GetForUpdate(tx, binding)
		if err != nil {
			return err
		}
		if current.AccountID == nil || *current.AccountID != *a.AccountID {
			return personalErrors.ErrRecoveryKeyState
		}
		result, err = readRecoveryResult(d, tx, current, nil)
		return err
	})
	if err != nil {
		return AccessResult{}, err
	}
	return AccessResult{Authorized: &result}, nil
}

func readRecoveryResult(d GetRecoveryStatusDependencies, tx shared.Transaction, a *attempt.AuthAttempt, input *CompleteRecoveryInput) (AuthorizedResult, error) {
	if a.Purpose != attempt.Recovery || a.State != attempt.Consumed || a.AccountID == nil || a.ResultReference == nil || a.ConsumedAt == nil || a.ProviderOperation == nil || a.ProviderOperation.Kind != attempt.Check {
		return AuthorizedResult{}, personalErrors.ErrRecoveryKeyState
	}
	if err := checkRecoveryOTPReceipt(d.Receipts, tx, a); err != nil {
		return AuthorizedResult{}, err
	}
	r, err := d.Receipts.Get(tx, receipt.Owner{AccountID: *a.AccountID, AttemptID: a.ID}, *a.ResultReference)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if r.Purpose != recoveryCompletePurpose || !r.CompletedAt.Equal(*a.ConsumedAt) {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	if input != nil {
		if input.Binding != a.Binding || input.OperationID != r.OperationID {
			return AuthorizedResult{}, personalErrors.ErrRecoveryKeyState
		}
		if err = r.Match(recoveryCompletePurpose, input.RequestHash); err != nil {
			return AuthorizedResult{}, err
		}
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
	var saved recoveryAccessResponse
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&saved); err != nil || decoder.Decode(new(any)) != io.EOF {
		return AuthorizedResult{}, personalErrors.ErrInvalidEnvelope
	}
	if saved.AttemptID != a.ID || saved.Candidate != a.Candidate || !deviceauthorization.ValidID(saved.ConsumedKeyID) || saved.Authorized.Status != "authorized" || saved.Authorized.RecoveryKit == nil {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	oldKey, err := d.Recovery.Get(tx, *a.AccountID, saved.ConsumedKeyID)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if oldKey.ConsumedAt == nil || !oldKey.ConsumedAt.Equal(r.CompletedAt) || oldKey.RevokedAt != nil {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	if input != nil && (input.RecoveryKeyID != oldKey.ID || d.RecoverySecrets.Verify(input.RecoverySecret, oldKey.Verifier) != nil) {
		return AuthorizedResult{}, personalErrors.ErrRecoveryKeyState
	}
	tokens := saved.Authorized.Session
	sid, err := uuid.Parse(tokens.SessionID)
	if err != nil || sid.String() != tokens.SessionID || !deviceauthorization.ValidID(sid) || r.ResultReference != sid || tokens.AccountID != a.AccountID.String() || tokens.DeviceID != a.Candidate.ID.String() {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	active, err := d.Sessions.SessionActive(tx, *a.AccountID, sid, a.Candidate.ID)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if !active {
		return AuthorizedResult{}, personalErrors.ErrSessionRevoked
	}
	dev, err := d.Sessions.GetDevice(tx, *a.AccountID, a.Candidate.ID)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if dev.PublicKey != a.Candidate.PublicKey || dev.Platform != a.Candidate.Platform || dev.Label != a.Candidate.Label || dev.AuthorizedByMethod != "recovery" || !dev.CreatedAt.Equal(r.CompletedAt) {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	sess, err := d.Sessions.GetSession(tx, *a.AccountID, sid)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if sess.DeviceID != a.Candidate.ID || !sess.CreatedAt.Equal(r.CompletedAt) {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	kit := saved.Authorized.RecoveryKit
	keyID, err := uuid.Parse(kit.ID)
	if err != nil || keyID.String() != kit.ID {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	current, err := d.Recovery.Get(tx, *a.AccountID, keyID)
	if errors.Is(err, personalErrors.ErrRecoveryKeyNotFound) {
		return AuthorizedResult{}, personalErrors.ErrRecoveryKeyState
	}
	if err != nil {
		return AuthorizedResult{}, err
	}
	if !current.Live() {
		return AuthorizedResult{}, personalErrors.ErrRecoveryKeyState
	}
	if kit.Generation != strconv.FormatInt(current.Generation, 10) || !current.CreatedAt.Equal(r.CompletedAt) || d.RecoverySecrets.Verify(kit.Secret, current.Verifier) != nil {
		return AuthorizedResult{}, personalErrors.ErrInvalidReceipt
	}
	return saved.Authorized, nil
}
