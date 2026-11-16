package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/refresh"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type RefreshSessionInput struct {
	OperationID                   uuid.UUID
	RequestHash                   [32]byte
	RefreshToken, DeviceSignature string
}

func (RefreshSessionInput) String() string      { return "personal refresh input (credentials redacted)" }
func (in RefreshSessionInput) GoString() string { return in.String() }

type RefreshSessionDependencies struct {
	UoW       shared.UnitOfWork
	Identity  repository.RefreshIdentityResolver
	Rotations repository.RefreshSessionRepository
	Sessions  repository.AuthSessionRepository
	Events    repository.SecurityEventRepository
	Tokens    security.AccessTokens
	Proofs    security.DeviceProofs
	Envelopes security.Envelopes
	Now       func() time.Time
}
type RefreshSession struct{ dependencies RefreshSessionDependencies }

func NewRefreshSession(d RefreshSessionDependencies) (*RefreshSession, error) {
	if d.UoW == nil || d.Identity == nil || d.Rotations == nil || d.Sessions == nil || d.Events == nil || d.Tokens == nil || d.Proofs == nil || d.Envelopes == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &RefreshSession{dependencies: d}, nil
}
func (u *RefreshSession) Execute(ctx context.Context, in RefreshSessionInput) (SessionTokens, error) {
	if err := shared.ValidatePersonalAnonymousScope(ctx); err != nil {
		return SessionTokens{}, err
	}
	if in.OperationID == uuid.Nil || in.RequestHash == [32]byte{} || !validRefreshSecret(in.RefreshToken) || len(in.DeviceSignature) != 86 {
		return SessionTokens{}, personalErrors.ErrInvalidRefresh
	}
	d := u.dependencies
	hash := sha256.Sum256([]byte(in.RefreshToken))
	accountID, err := d.Identity.Resolve(ctx, hash)
	if err != nil {
		return SessionTokens{}, err
	}
	ctx, err = shared.WithAuthenticatedPersonalAccount(ctx, accountID)
	if err != nil {
		return SessionTokens{}, err
	}
	var result SessionTokens
	var rejected error
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		s, old, err := d.Rotations.GetForUpdate(tx, accountID, hash)
		if err != nil {
			return err
		}
		if err = requirePersonalSession(d.Sessions, tx, security.AccessClaims{AccountID: accountID, SessionID: s.ID, DeviceID: s.DeviceID}); err != nil {
			return err
		}
		device, err := d.Sessions.GetDevice(tx, accountID, s.DeviceID)
		if err != nil {
			return err
		}
		generation := s.Generation
		if old != nil {
			generation = old.Generation
		}
		proof := security.RefreshProof{OperationID: in.OperationID, SessionID: s.ID, Generation: generation, RefreshHash: hash}
		if err = d.Proofs.VerifyRefresh(device.PublicKey[:], in.DeviceSignature, proof); err != nil {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		if old != nil {
			if old.Match(in.OperationID, in.RequestHash) != nil {
				// Persist the rejection only after a valid device signature. An
				// untrusted request never revokes or rotates a session family.
				if err = d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: accountID, ActorDeviceID: s.DeviceID, OperationID: in.OperationID, Type: "refresh_replay_rejected", OccurredAt: now}); err != nil {
					return err
				}
				rejected = personalErrors.ErrInvalidProof
				return nil
			}
			envelope, err := old.ReadResponse(now)
			if err != nil {
				return err
			}
			plain, err := d.Envelopes.OpenReceipt(old.Context(), envelope)
			if err != nil {
				return err
			}
			decoder := json.NewDecoder(bytes.NewReader(plain))
			decoder.DisallowUnknownFields()
			if err = decoder.Decode(&result); err != nil || result.AccountID != accountID.String() || result.SessionID != s.ID.String() || result.DeviceID != s.DeviceID.String() || result.Generation != strconv.FormatInt(old.Generation+1, 10) || !validRefreshSecret(result.RefreshToken) || !validRefreshSecret(result.RevocationTicket) {
				return personalErrors.ErrInvalidEnvelope
			}
			return nil
		}
		rotation, err := refresh.NewRotation(s, in.OperationID, in.RequestHash, now)
		if err != nil {
			return err
		}
		next, err := randomAccessSecret()
		if err != nil {
			return err
		}
		ticket, err := randomAccessSecret()
		if err != nil {
			return err
		}
		if err = s.Rotate(sha256.Sum256([]byte(next)), sha256.Sum256([]byte(ticket)), now); err != nil {
			return err
		}
		access, claims, err := d.Tokens.Issue(accountID, s.ID, s.DeviceID, now)
		if err != nil {
			return err
		}
		result = SessionTokens{AccountID: accountID.String(), DeviceID: s.DeviceID.String(), SessionID: s.ID.String(), AccessToken: access, AccessExpiresAt: accessTimestamp(claims.ExpiresAt), RefreshToken: next, RefreshExpiresAt: accessTimestamp(s.ExpiresAt), Generation: strconv.FormatInt(s.Generation, 10), RevocationTicket: ticket}
		plain, err := json.Marshal(result)
		if err != nil {
			return personalErrors.ErrInvalidRefresh
		}
		envelope, err := d.Envelopes.SealReceipt(rotation.Context(), plain)
		if err != nil {
			return err
		}
		if err = rotation.SetResponse(envelope); err != nil {
			return err
		}
		if err = d.Rotations.SaveRotation(tx, accountID, s, rotation); err != nil {
			return err
		}
		return d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: accountID, ActorDeviceID: s.DeviceID, OperationID: in.OperationID, Type: "session_refreshed", OccurredAt: now})
	})
	if err != nil {
		return SessionTokens{}, err
	}
	if rejected != nil {
		return SessionTokens{}, rejected
	}
	return result, nil
}
func validRefreshSecret(s string) bool {
	if len(s) < 32 || len(s) > 512 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}
