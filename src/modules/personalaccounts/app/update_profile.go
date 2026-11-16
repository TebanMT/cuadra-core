package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

type UpdateProfileDependencies struct {
	UoW           shared.UnitOfWork
	Accounts      repository.AccountRepository
	Commands      protocolRepository.Commands
	Canonicalizer protocol.Canonicalizer
	Now           func() time.Time
}
type UpdateProfile struct{ dependencies UpdateProfileDependencies }

func NewUpdateProfile(d UpdateProfileDependencies) (*UpdateProfile, error) {
	if d.UoW == nil || d.Accounts == nil || d.Commands == nil || d.Canonicalizer == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &UpdateProfile{dependencies: d}, nil
}
func (u *UpdateProfile) Execute(ctx context.Context, actor security.AccessClaims, in account.ProfileCommand) (protocol.OperationResult, error) {
	if !validPersonalActor(actor) {
		return protocol.OperationResult{}, personalErrors.ErrInvalidAccessToken
	}
	if err := in.ValidateEnvelope(); err != nil {
		return protocol.OperationResult{}, err
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return protocol.OperationResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := json.Marshal(in)
	if err != nil {
		return protocol.OperationResult{}, protocol.ErrCommandInvalid
	}
	d := u.dependencies
	canonical, err := d.Canonicalizer.Canonicalize(raw)
	if err != nil {
		return protocol.OperationResult{}, protocol.ErrCommandInvalid
	}
	hash := sha256.Sum256(canonical)
	op, origin := uuid.MustParse(in.OperationID), uuid.MustParse(in.OriginDeviceID)
	var result protocol.OperationResult
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := d.Commands.RequireActor(tx, protocol.Actor{AccountID: actor.AccountID, SessionID: actor.SessionID, DeviceID: actor.DeviceID}, origin); err != nil {
			return err
		}
		previous, err := d.Commands.GetReceipt(tx, actor.AccountID, op)
		if err == nil {
			if err = previous.Match(hash, in.Type, origin); err != nil {
				return err
			}
			result = previous.Result
			return nil
		}
		if !errors.Is(err, protocol.ErrOperationMissing) {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		finish := func(changes []protocol.Change) error {
			return d.Commands.Commit(tx, protocol.Receipt{OperationID: op, AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID, OriginDeviceID: origin, RequestHash: hash, CommandType: in.Type, CapturedAt: in.CapturedAt, Result: result, CommittedAt: now}, changes)
		}
		reject := func() error {
			result = protocol.Failure(op, "rejected", "VALIDATION_ERROR", "Revisa el nombre, las unidades y la revisión del perfil.", "correct", false)
			return finish(nil)
		}
		if in.EntityID != actor.AccountID.String() || account.ValidateProfile(in.Payload.DisplayName, in.Payload.WeightUnit) != nil {
			return reject()
		}
		var base int64
		if in.Base.Revision != nil {
			base, err = protocol.ParseRevision(*in.Base.Revision)
		} else {
			parent, parentErr := d.Commands.GetReceipt(tx, actor.AccountID, uuid.MustParse(*in.Base.AfterOperationID))
			if errors.Is(parentErr, protocol.ErrOperationMissing) {
				result = protocol.Failure(op, "dependency_pending", "DEPENDENCY_PENDING", "Falta confirmar el cambio anterior.", "wait", true)
				return nil
			}
			if parentErr != nil {
				return parentErr
			}
			if parent.Result.Outcome != "accepted" && parent.Result.Outcome != "accepted_with_review" {
				result = protocol.Failure(op, "dependency_blocked", "DEPENDENCY_BLOCKED", "Revisa el cambio anterior antes de continuar.", "review", false)
				return finish(nil)
			}
			matches := 0
			for _, affected := range parent.Result.Affected {
				if affected.EntityType == "profile" && affected.EntityID == in.EntityID {
					matches++
					base, err = protocol.ParseRevision(affected.Revision)
				}
			}
			if matches != 1 {
				return reject()
			}
		}
		if err != nil || base < 1 {
			return reject()
		}
		current, err := d.Accounts.GetByID(tx, actor.AccountID)
		if err != nil {
			return err
		}
		if current.Revision != base {
			conflict := protocol.Conflict{ID: uuid.New(), AccountID: actor.AccountID, OperationID: op, EntityType: "profile", EntityID: actor.AccountID, BaseRevision: base, CurrentRevision: current.Revision, Proposal: canonical, Reason: "revision", CreatedAt: now}
			if err = d.Commands.SaveConflict(tx, conflict); err != nil {
				return err
			}
			change, err := conflict.Change()
			if err != nil {
				return err
			}
			result = protocol.Failure(op, "conflict", "REVISION_CONFLICT", "El perfil cambió en otro dispositivo. Conservamos tu propuesta.", "review", false)
			result.ConflictID = conflict.ID.String()
			return finish([]protocol.Change{change})
		}
		if err = current.UpdateProfile(in.Payload.DisplayName, in.Payload.WeightUnit, base, now); err != nil {
			return err
		}
		updated, err := d.Accounts.UpdateProfile(tx, actor.AccountID, current, base)
		if err != nil {
			return err
		}
		profile := struct {
			ID          string             `json:"id"`
			DisplayName string             `json:"display_name"`
			WeightUnit  account.WeightUnit `json:"weight_unit"`
			Revision    string             `json:"revision"`
		}{ID: updated.ID.String(), DisplayName: updated.DisplayName, WeightUnit: updated.WeightUnit, Revision: strconv.FormatInt(updated.Revision, 10)}
		data, err := json.Marshal(profile)
		if err != nil {
			return err
		}
		result = protocol.OperationResult{OperationID: op.String(), Outcome: "accepted", Affected: []protocol.Affected{{EntityType: "profile", EntityID: in.EntityID, Revision: profile.Revision}}, CommittedAt: protocol.Timestamp(now), Result: &struct{}{}}
		return finish([]protocol.Change{{EntityType: "profile", EntityID: in.EntityID, Revision: profile.Revision, Data: data}})
	})
	if err != nil {
		return protocol.OperationResult{}, err
	}
	return result, nil
}
