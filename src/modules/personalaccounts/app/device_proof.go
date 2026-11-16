package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceproof"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type DeviceProofResult struct {
	ID          string `json:"id"`
	Nonce       string `json:"nonce"`
	Purpose     string `json:"purpose"`
	RequestHash string `json:"request_hash"`
	ExpiresAt   string `json:"expires_at"`
}
type ProofTokenResult struct {
	ProofToken string `json:"proof_token"`
	ExpiresAt  string `json:"expires_at"`
}

func (ProofTokenResult) String() string     { return "personal proof result (token redacted)" }
func (p ProofTokenResult) GoString() string { return p.String() }

type StartDeviceProofInput struct {
	OperationID       uuid.UUID
	RequestHash       [32]byte
	ProofID           uuid.UUID
	Purpose           security.ProofPurpose
	TargetRequestHash [32]byte
}
type StartDeviceProofDependencies struct {
	UoW      shared.UnitOfWork
	Sessions repository.AuthSessionRepository
	Proofs   repository.DeviceProofRepository
	Receipts repository.AuthReceiptRepository
	Events   repository.SecurityEventRepository
	Now      func() time.Time
}
type StartDeviceProof struct{ dependencies StartDeviceProofDependencies }

func NewStartDeviceProof(d StartDeviceProofDependencies) (*StartDeviceProof, error) {
	if d.UoW == nil || d.Sessions == nil || d.Proofs == nil || d.Receipts == nil || d.Events == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &StartDeviceProof{dependencies: d}, nil
}

func (u *StartDeviceProof) Execute(ctx context.Context, actor security.AccessClaims, in StartDeviceProofInput) (DeviceProofResult, error) {
	if !validPersonalActor(actor) {
		return DeviceProofResult{}, personalErrors.ErrInvalidAccessToken
	}
	if in.OperationID == uuid.Nil || in.RequestHash == [32]byte{} || in.ProofID == uuid.Nil || in.TargetRequestHash == [32]byte{} {
		return DeviceProofResult{}, personalErrors.ErrInvalidProof
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return DeviceProofResult{}, err
	}
	d := u.dependencies
	var result DeviceProofResult
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
			return err
		}
		owner := receipt.Owner{AccountID: actor.AccountID}
		previous, err := d.Receipts.Get(tx, owner, in.OperationID)
		if err == nil {
			if err = previous.Match("device_proof_start", in.RequestHash); err != nil {
				return err
			}
			if previous.ResultReference != in.ProofID {
				return personalErrors.ErrIdempotencyMismatch
			}
			p, err := d.Proofs.GetForUpdate(tx, actor, in.ProofID)
			if err != nil {
				return err
			}
			if p.Purpose != in.Purpose || subtle.ConstantTimeCompare(p.TargetRequestHash[:], in.TargetRequestHash[:]) != 1 {
				return personalErrors.ErrIdempotencyMismatch
			}
			if !p.ValidAt(d.Now()) {
				return personalErrors.ErrResultExpired
			}
			result = deviceProofResult(p)
			return nil
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		var nonce [32]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return personalErrors.ErrInvalidSecurityConfiguration
		}
		p, err := deviceproof.New(actor, in.ProofID, in.Purpose, in.TargetRequestHash, nonce, now)
		if err != nil {
			return err
		}
		if err = d.Proofs.Create(tx, actor, p); err != nil {
			return err
		}
		r, err := receipt.New(in.OperationID, owner, "device_proof_start", in.RequestHash, now)
		if err != nil {
			return err
		}
		r.ResultReference = p.ID
		if err = d.Receipts.Create(tx, r); err != nil {
			return err
		}
		if err = d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID, OperationID: in.OperationID, Type: "device_proof_started", OccurredAt: now}); err != nil {
			return err
		}
		result = deviceProofResult(p)
		return nil
	})
	if err != nil {
		return DeviceProofResult{}, err
	}
	return result, nil
}
func deviceProofResult(p *deviceproof.Proof) DeviceProofResult {
	return DeviceProofResult{ID: p.ID.String(), Nonce: base64.RawURLEncoding.EncodeToString(p.Nonce[:]), Purpose: string(p.Purpose), RequestHash: hex.EncodeToString(p.TargetRequestHash[:]), ExpiresAt: accessTimestamp(p.ExpiresAt)}
}

type CompleteDeviceProofInput struct {
	OperationID uuid.UUID
	RequestHash [32]byte
	ProofID     uuid.UUID
	Signature   string
}
type CompleteDeviceProofDependencies struct {
	UoW       shared.UnitOfWork
	Sessions  repository.AuthSessionRepository
	Proofs    repository.DeviceProofRepository
	Receipts  repository.AuthReceiptRepository
	Events    repository.SecurityEventRepository
	Verifier  security.DeviceProofs
	Envelopes security.Envelopes
	Now       func() time.Time
}
type CompleteDeviceProof struct {
	dependencies CompleteDeviceProofDependencies
}

func NewCompleteDeviceProof(d CompleteDeviceProofDependencies) (*CompleteDeviceProof, error) {
	if d.UoW == nil || d.Sessions == nil || d.Proofs == nil || d.Receipts == nil || d.Events == nil || d.Verifier == nil || d.Envelopes == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &CompleteDeviceProof{dependencies: d}, nil
}
func (u *CompleteDeviceProof) Execute(ctx context.Context, actor security.AccessClaims, in CompleteDeviceProofInput) (ProofTokenResult, error) {
	if !validPersonalActor(actor) {
		return ProofTokenResult{}, personalErrors.ErrInvalidAccessToken
	}
	if in.OperationID == uuid.Nil || in.RequestHash == [32]byte{} || in.ProofID == uuid.Nil || len(in.Signature) != 86 {
		return ProofTokenResult{}, personalErrors.ErrInvalidProof
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return ProofTokenResult{}, err
	}
	d := u.dependencies
	var result ProofTokenResult
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := requirePersonalSession(d.Sessions, tx, actor); err != nil {
			return err
		}
		// Fetch by exact actor before touching the receipt or its envelope.
		p, err := d.Proofs.GetForUpdate(tx, actor, in.ProofID)
		if err != nil {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		if !p.ValidAt(now) {
			return personalErrors.ErrResultExpired
		}
		device, err := d.Sessions.GetDevice(tx, actor.AccountID, actor.DeviceID)
		if err != nil {
			return err
		}
		if err = d.Verifier.VerifyDevice(device.PublicKey[:], in.Signature, p.SigningProof(), now); err != nil {
			return err
		}
		owner := receipt.Owner{AccountID: actor.AccountID}
		previous, err := d.Receipts.Get(tx, owner, in.OperationID)
		if err == nil {
			if err = previous.Match("device_proof_complete", in.RequestHash); err != nil {
				return err
			}
			if previous.ResultReference != p.ID || p.TokenHash == nil {
				return personalErrors.ErrIdempotencyMismatch
			}
			envelope, err := previous.ReadResponse(now)
			if err != nil {
				return err
			}
			plain, err := d.Envelopes.OpenReceipt(previous.Context(), envelope)
			if err != nil {
				return err
			}
			decoder := json.NewDecoder(bytes.NewReader(plain))
			decoder.DisallowUnknownFields()
			if err = decoder.Decode(&result); err != nil || !validRefreshSecret(result.ProofToken) || result.ExpiresAt != accessTimestamp(p.ExpiresAt) {
				return personalErrors.ErrInvalidEnvelope
			}
			hash := sha256.Sum256([]byte(result.ProofToken))
			if subtle.ConstantTimeCompare(hash[:], p.TokenHash[:]) != 1 {
				return personalErrors.ErrInvalidEnvelope
			}
			return nil
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		token, err := randomAccessSecret()
		if err != nil {
			return err
		}
		if err = p.Verify(sha256.Sum256([]byte(token)), now); err != nil {
			return err
		}
		if err = d.Proofs.SaveVerified(tx, actor, p); err != nil {
			return err
		}
		r, err := receipt.New(in.OperationID, owner, "device_proof_complete", in.RequestHash, now)
		if err != nil {
			return err
		}
		result = ProofTokenResult{ProofToken: token, ExpiresAt: accessTimestamp(p.ExpiresAt)}
		plain, err := json.Marshal(result)
		if err != nil {
			return personalErrors.ErrInvalidProof
		}
		envelope, err := d.Envelopes.SealReceipt(r.Context(), plain)
		if err != nil {
			return err
		}
		if err = r.SetResponse(envelope, p.ExpiresAt, p.ID); err != nil {
			return err
		}
		if err = d.Receipts.Create(tx, r); err != nil {
			return err
		}
		return d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID, OperationID: in.OperationID, Type: "device_proof_verified", OccurredAt: now})
	})
	if err != nil {
		return ProofTokenResult{}, err
	}
	return result, nil
}
