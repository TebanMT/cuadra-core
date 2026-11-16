package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/recovery"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type RotateRecoveryKeyInput struct {
	OperationID uuid.UUID
	RequestHash [32]byte
	KeyID       uuid.UUID
	ProofToken  string
}

func (RotateRecoveryKeyInput) String() string {
	return "personal recovery rotation (proof token redacted)"
}
func (in RotateRecoveryKeyInput) GoString() string { return in.String() }

type RotateRecoveryKeyDependencies struct {
	UoW             shared.UnitOfWork
	Sessions        repository.AuthSessionRepository
	Recovery        repository.RecoveryKeyRepository
	Proofs          repository.DeviceProofRepository
	Receipts        repository.AuthReceiptRepository
	Events          repository.SecurityEventRepository
	RecoverySecrets security.RecoveryKeys
	Envelopes       security.Envelopes
	Now             func() time.Time
}
type RotateRecoveryKey struct{ dependencies RotateRecoveryKeyDependencies }

func NewRotateRecoveryKey(d RotateRecoveryKeyDependencies) (*RotateRecoveryKey, error) {
	if d.UoW == nil || d.Sessions == nil || d.Recovery == nil || d.Proofs == nil || d.Receipts == nil || d.Events == nil || d.RecoverySecrets == nil || d.Envelopes == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &RotateRecoveryKey{dependencies: d}, nil
}

// This envelope is private persistence, not an HTTP shape. Actor identity is
// authenticated by the envelope even though receipt ownership is account-wide.
type recoveryRotationResponse struct {
	SessionID uuid.UUID   `json:"session_id"`
	DeviceID  uuid.UUID   `json:"device_id"`
	Kit       RecoveryKit `json:"kit"`
}

func (recoveryRotationResponse) String() string     { return "recovery rotation receipt (secret redacted)" }
func (r recoveryRotationResponse) GoString() string { return r.String() }

func (u *RotateRecoveryKey) Execute(ctx context.Context, actor security.AccessClaims, in RotateRecoveryKeyInput) (RecoveryKit, error) {
	if !validPersonalActor(actor) {
		return RecoveryKit{}, personalErrors.ErrInvalidAccessToken
	}
	if in.OperationID == uuid.Nil || in.KeyID == uuid.Nil || in.RequestHash == [32]byte{} {
		return RecoveryKit{}, personalErrors.ErrInvalidRecoveryKey
	}
	if !validRefreshSecret(in.ProofToken) {
		return RecoveryKit{}, personalErrors.ErrInvalidProof
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return RecoveryKit{}, err
	}
	d := u.dependencies
	var result RecoveryKit
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
			return err
		}
		owner := receipt.Owner{AccountID: actor.AccountID}
		previous, err := d.Receipts.Get(tx, owner, in.OperationID)
		if err == nil {
			if err = previous.Match("recovery_key_rotate", in.RequestHash); err != nil {
				return err
			}
			if previous.ResultReference != in.KeyID {
				return personalErrors.ErrIdempotencyMismatch
			}
			result, err = readRecoveryRotationResponse(d.Recovery, d.RecoverySecrets, d.Envelopes, d.Now, tx, actor, previous)
			return err
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		if err = d.Proofs.Consume(tx, actor, sha256.Sum256([]byte(in.ProofToken)), security.RecoveryKey, in.RequestHash, in.OperationID, now); err != nil {
			return err
		}
		generation, err := d.Recovery.NextGeneration(tx, actor.AccountID)
		if err != nil {
			return err
		}
		previousKey, err := d.Recovery.GetLive(tx, actor.AccountID)
		if err != nil && !errors.Is(err, personalErrors.ErrRecoveryKeyNotFound) {
			return err
		}
		if err == nil {
			if err = previousKey.Revoke(now); err != nil {
				return err
			}
			if err = d.Recovery.SaveState(tx, actor.AccountID, previousKey); err != nil {
				return err
			}
		}
		secret, err := randomAccessSecret()
		if err != nil {
			return err
		}
		verifier, err := d.RecoverySecrets.CreateVerifier(secret)
		if err != nil {
			return err
		}
		key, err := recovery.NewKey(actor.AccountID, in.KeyID, generation, verifier, now)
		if err != nil {
			return err
		}
		if err = d.Recovery.Create(tx, actor.AccountID, key); err != nil {
			return err
		}
		result = RecoveryKit{ID: key.ID.String(), Secret: secret, Generation: strconv.FormatInt(generation, 10)}
		r, err := receipt.New(in.OperationID, owner, "recovery_key_rotate", in.RequestHash, now)
		if err != nil {
			return err
		}
		plain, err := json.Marshal(recoveryRotationResponse{SessionID: actor.SessionID, DeviceID: actor.DeviceID, Kit: result})
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
		return d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID, OperationID: in.OperationID, Type: "recovery_key_rotated", OccurredAt: now})
	})
	if err != nil {
		return RecoveryKit{}, err
	}
	return result, nil
}

func readRecoveryRotationResponse(keys repository.RecoveryKeyRepository, secrets security.RecoveryKeys, envelopes security.Envelopes, now func() time.Time, tx shared.Transaction, actor security.AccessClaims, r *receipt.Receipt) (RecoveryKit, error) {
	envelope, err := r.ReadResponse(now())
	if err != nil {
		return RecoveryKit{}, err
	}
	plain, err := envelopes.OpenReceipt(r.Context(), envelope)
	if err != nil {
		return RecoveryKit{}, err
	}
	defer clear(plain)
	if len(plain) > 16384 {
		return RecoveryKit{}, personalErrors.ErrInvalidEnvelope
	}
	var saved recoveryRotationResponse
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&saved) != nil || decoder.Decode(new(any)) != io.EOF || saved.Kit.ID != r.ResultReference.String() || !validRefreshSecret(saved.Kit.Secret) {
		return RecoveryKit{}, personalErrors.ErrInvalidEnvelope
	}
	if saved.SessionID != actor.SessionID || saved.DeviceID != actor.DeviceID {
		return RecoveryKit{}, personalErrors.ErrInvalidProof
	}
	key, err := keys.Get(tx, actor.AccountID, r.ResultReference)
	if errors.Is(err, personalErrors.ErrRecoveryKeyNotFound) {
		return RecoveryKit{}, personalErrors.ErrRecoveryKeyState
	}
	if err != nil {
		return RecoveryKit{}, err
	}
	if !key.Live() {
		return RecoveryKit{}, personalErrors.ErrRecoveryKeyState
	}
	if saved.Kit.Generation != strconv.FormatInt(key.Generation, 10) || !key.CreatedAt.Equal(r.CompletedAt) || secrets.Verify(saved.Kit.Secret, key.Verifier) != nil {
		return RecoveryKit{}, personalErrors.ErrInvalidEnvelope
	}
	return saved.Kit, nil
}
