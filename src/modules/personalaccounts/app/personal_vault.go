package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/vault"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type VaultResult struct {
	ID         string `json:"id"`
	KeyVersion string `json:"key_version"`
	DataKey    string `json:"data_key"`
}

func (VaultResult) String() string     { return "personal vault response (data key redacted)" }
func (v VaultResult) GoString() string { return v.String() }

type CreatePersonalVaultInput struct {
	OperationID uuid.UUID
	RequestHash [32]byte
	VaultID     uuid.UUID
}

type UnlockPersonalVaultInput struct {
	OperationID uuid.UUID
	RequestHash [32]byte
	VaultID     uuid.UUID
	ProofToken  string
}

func (UnlockPersonalVaultInput) String() string {
	return "personal vault unlock (proof token redacted)"
}
func (in UnlockPersonalVaultInput) GoString() string { return in.String() }

type CreatePersonalVaultDependencies struct {
	UoW       shared.UnitOfWork
	Sessions  repository.AuthSessionRepository
	Vaults    repository.VaultRepository
	Receipts  repository.AuthReceiptRepository
	Events    repository.SecurityEventRepository
	Envelopes security.Envelopes
	Now       func() time.Time
}

type UnlockPersonalVaultDependencies struct {
	UoW       shared.UnitOfWork
	Sessions  repository.AuthSessionRepository
	Vaults    repository.VaultRepository
	Proofs    repository.DeviceProofRepository
	Receipts  repository.AuthReceiptRepository
	Events    repository.SecurityEventRepository
	Envelopes security.Envelopes
	Now       func() time.Time
}

type CreatePersonalVault struct {
	dependencies CreatePersonalVaultDependencies
}
type UnlockPersonalVault struct {
	dependencies UnlockPersonalVaultDependencies
}

func validVaultDependencies(d CreatePersonalVaultDependencies) bool {
	return d.UoW != nil && d.Sessions != nil && d.Vaults != nil && d.Receipts != nil && d.Events != nil && d.Envelopes != nil && d.Now != nil
}

func NewCreatePersonalVault(d CreatePersonalVaultDependencies) (*CreatePersonalVault, error) {
	if !validVaultDependencies(d) {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &CreatePersonalVault{dependencies: d}, nil
}

func (d UnlockPersonalVaultDependencies) common() CreatePersonalVaultDependencies {
	return CreatePersonalVaultDependencies{UoW: d.UoW, Sessions: d.Sessions, Vaults: d.Vaults, Receipts: d.Receipts, Events: d.Events, Envelopes: d.Envelopes, Now: d.Now}
}

func NewUnlockPersonalVault(d UnlockPersonalVaultDependencies) (*UnlockPersonalVault, error) {
	if !validVaultDependencies(d.common()) || d.Proofs == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &UnlockPersonalVault{dependencies: d}, nil
}

func validVaultRequest(operationID, vaultID uuid.UUID, hash [32]byte) bool {
	valid := func(id uuid.UUID) bool {
		return id != uuid.Nil && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
	}
	return valid(operationID) && valid(vaultID) && hash != [32]byte{}
}

func (u *CreatePersonalVault) Execute(ctx context.Context, actor security.AccessClaims, in CreatePersonalVaultInput) (VaultResult, error) {
	if !validPersonalActor(actor) {
		return VaultResult{}, personalErrors.ErrInvalidAccessToken
	}
	if !validVaultRequest(in.OperationID, in.VaultID, in.RequestHash) {
		return VaultResult{}, personalErrors.ErrVaultState
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return VaultResult{}, err
	}
	d := u.dependencies
	var result VaultResult
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
			return err
		}
		previous, err := d.Receipts.Get(tx, receipt.Owner{AccountID: actor.AccountID}, in.OperationID)
		if err == nil {
			result, err = replayVaultResponse(d, tx, actor, previous, "vault_create", in.VaultID, in.RequestHash)
			return err
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		dataKey := make([]byte, 32)
		defer clear(dataKey)
		if _, err = io.ReadFull(rand.Reader, dataKey); err != nil {
			return personalErrors.ErrInvalidEnvelope
		}
		envelope, err := d.Envelopes.WrapVault(security.VaultContext{AccountID: actor.AccountID, VaultID: in.VaultID}, dataKey)
		if err != nil {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		v, err := vault.New(actor.AccountID, in.VaultID, envelope, now)
		if err != nil {
			return err
		}
		if err = d.Vaults.Create(tx, actor.AccountID, v); err != nil {
			return err
		}
		result = VaultResult{ID: v.ID.String(), KeyVersion: v.Envelope.KeyVersion, DataKey: base64.RawURLEncoding.EncodeToString(dataKey)}
		return saveVaultResponse(d, tx, actor, result, in.OperationID, in.RequestHash, "vault_create", "vault_created", now)
	})
	if err != nil {
		return VaultResult{}, err
	}
	return result, nil
}

func (u *UnlockPersonalVault) Execute(ctx context.Context, actor security.AccessClaims, in UnlockPersonalVaultInput) (VaultResult, error) {
	if !validPersonalActor(actor) {
		return VaultResult{}, personalErrors.ErrInvalidAccessToken
	}
	if !validVaultRequest(in.OperationID, in.VaultID, in.RequestHash) {
		return VaultResult{}, personalErrors.ErrVaultState
	}
	if !validRefreshSecret(in.ProofToken) {
		return VaultResult{}, personalErrors.ErrInvalidProof
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return VaultResult{}, err
	}
	d := u.dependencies
	var result VaultResult
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
			return err
		}
		previous, err := d.Receipts.Get(tx, receipt.Owner{AccountID: actor.AccountID}, in.OperationID)
		if err == nil {
			result, err = replayVaultResponse(d.common(), tx, actor, previous, "vault_unlock", in.VaultID, in.RequestHash)
			return err
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		v, err := d.Vaults.Get(tx, actor.AccountID, in.VaultID)
		if err != nil {
			return err
		}
		if !v.Live() {
			return personalErrors.ErrVaultState
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		if err = d.Proofs.Consume(tx, actor, sha256.Sum256([]byte(in.ProofToken)), security.VaultUnlock, in.RequestHash, in.OperationID, now); err != nil {
			return err
		}
		dataKey, err := d.Envelopes.UnwrapVault(v.Context(), v.Envelope)
		if err != nil {
			return err
		}
		defer clear(dataKey)
		if len(dataKey) != 32 {
			return personalErrors.ErrInvalidEnvelope
		}
		result = VaultResult{ID: v.ID.String(), KeyVersion: v.Envelope.KeyVersion, DataKey: base64.RawURLEncoding.EncodeToString(dataKey)}
		return saveVaultResponse(d.common(), tx, actor, result, in.OperationID, in.RequestHash, "vault_unlock", "vault_unlocked", now)
	})
	if err != nil {
		return VaultResult{}, err
	}
	return result, nil
}

// The receipt's account-wide AAD is complemented by encrypted actor identity.
// The permanent vault envelope and this ten-minute response are independent.
type personalVaultResponse struct {
	SessionID uuid.UUID   `json:"session_id"`
	DeviceID  uuid.UUID   `json:"device_id"`
	Vault     VaultResult `json:"vault"`
}

func (personalVaultResponse) String() string     { return "personal vault receipt (data key redacted)" }
func (v personalVaultResponse) GoString() string { return v.String() }

func saveVaultResponse(d CreatePersonalVaultDependencies, tx shared.Transaction, actor security.AccessClaims, result VaultResult, operationID uuid.UUID, hash [32]byte, purpose, eventType string, now time.Time) error {
	r, err := receipt.New(operationID, receipt.Owner{AccountID: actor.AccountID}, purpose, hash, now)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(personalVaultResponse{SessionID: actor.SessionID, DeviceID: actor.DeviceID, Vault: result})
	if err != nil {
		return personalErrors.ErrInvalidReceipt
	}
	defer clear(plain)
	envelope, err := d.Envelopes.SealReceipt(r.Context(), plain)
	if err != nil {
		return err
	}
	if err = r.SetResponse(envelope, now.Add(10*time.Minute), uuid.MustParse(result.ID)); err != nil {
		return err
	}
	if err = d.Receipts.Create(tx, r); err != nil {
		return err
	}
	return d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID, OperationID: operationID, Type: eventType, OccurredAt: now})
}

func replayVaultResponse(d CreatePersonalVaultDependencies, tx shared.Transaction, actor security.AccessClaims, r *receipt.Receipt, purpose string, vaultID uuid.UUID, hash [32]byte) (VaultResult, error) {
	if err := r.Match(purpose, hash); err != nil {
		return VaultResult{}, err
	}
	if r.ResultReference != vaultID {
		return VaultResult{}, personalErrors.ErrIdempotencyMismatch
	}
	envelope, err := r.ReadResponse(d.Now())
	if err != nil {
		return VaultResult{}, err
	}
	plain, err := d.Envelopes.OpenReceipt(r.Context(), envelope)
	if err != nil {
		return VaultResult{}, err
	}
	defer clear(plain)
	if len(plain) > 4096 {
		return VaultResult{}, personalErrors.ErrInvalidEnvelope
	}
	var saved personalVaultResponse
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&saved); err != nil || decoder.Decode(new(any)) != io.EOF || saved.Vault.ID != vaultID.String() {
		return VaultResult{}, personalErrors.ErrInvalidEnvelope
	}
	if saved.SessionID != actor.SessionID || saved.DeviceID != actor.DeviceID {
		return VaultResult{}, personalErrors.ErrInvalidProof
	}
	dataKey, err := base64.RawURLEncoding.Strict().DecodeString(saved.Vault.DataKey)
	defer clear(dataKey)
	if err != nil || len(dataKey) != 32 || len(saved.Vault.DataKey) != 43 {
		return VaultResult{}, personalErrors.ErrInvalidEnvelope
	}
	v, err := d.Vaults.Get(tx, actor.AccountID, vaultID)
	if err != nil {
		return VaultResult{}, err
	}
	if !v.Live() {
		return VaultResult{}, personalErrors.ErrVaultState
	}
	if saved.Vault.KeyVersion != v.Envelope.KeyVersion {
		return VaultResult{}, personalErrors.ErrInvalidEnvelope
	}
	// A valid response retry need not consume another proof or extend its TTL.
	return saved.Vault, nil
}
