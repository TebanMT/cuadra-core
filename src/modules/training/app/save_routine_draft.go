package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

type SaveRoutineDraftDependencies struct {
	UoW           shared.UnitOfWork
	Routines      repository.RoutineRepository
	Drafts        repository.RoutineDraftRepository
	Commands      protocolRepository.Commands
	Stream        protocolRepository.Stream
	Canonicalizer protocol.Canonicalizer
	Now           func() time.Time
}
type SaveRoutineDraft struct{ d SaveRoutineDraftDependencies }

func NewSaveRoutineDraft(d SaveRoutineDraftDependencies) (*SaveRoutineDraft, error) {
	if d.UoW == nil || d.Routines == nil || d.Drafts == nil || d.Commands == nil || d.Stream == nil || d.Canonicalizer == nil || d.Now == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &SaveRoutineDraft{d: d}, nil
}

func (u *SaveRoutineDraft) Execute(ctx context.Context, actor security.AccessClaims, in routine.SaveDraftCommand) (protocol.OperationResult, error) {
	if !protocol.ValidID(actor.AccountID) || !protocol.ValidID(actor.SessionID) || !protocol.ValidID(actor.DeviceID) {
		return protocol.OperationResult{}, protocol.ErrCommandActor
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return protocol.OperationResult{}, err
	}
	if err = in.ValidateEnvelope(); err != nil {
		return protocol.OperationResult{}, err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return protocol.OperationResult{}, protocol.ErrCommandInvalid
	}
	d := u.d
	canonical, err := d.Canonicalizer.Canonicalize(raw)
	if err != nil {
		return protocol.OperationResult{}, protocol.ErrCommandInvalid
	}
	if len(canonical) > 1048576 {
		return protocol.OperationResult{}, trainingErrors.ErrTrainingLimit
	}
	hash := sha256.Sum256(canonical)
	op, origin, id := uuid.MustParse(in.OperationID), uuid.MustParse(in.OriginDeviceID), uuid.MustParse(in.EntityID)
	var baseVersionID *uuid.UUID
	if in.Payload.BaseVersionID != nil {
		value := uuid.MustParse(*in.Payload.BaseVersionID)
		baseVersionID = &value
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
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
		reject := func(code, message string) error {
			result = protocol.Failure(op, "rejected", code, message, "correct", false)
			return finish(nil)
		}
		base, dependency, err := resolveTrainingBase(tx, d.Commands, actor.AccountID, op, in.Base, "routine_draft", in.EntityID)
		if err != nil {
			return err
		}
		if dependency != nil {
			result = *dependency
			if result.Outcome == "dependency_pending" {
				return nil
			}
			return finish(nil)
		}
		if base < 1 {
			return reject("VALIDATION_ERROR", "Revisa la revisión del borrador.")
		}
		root, err := d.Routines.GetByID(tx, actor.AccountID, id)
		if errors.Is(err, trainingErrors.ErrRoutineNotFound) {
			return reject("NOT_FOUND", "La rutina no está disponible.")
		}
		if err != nil {
			return err
		}
		current, err := d.Routines.GetDraftByID(tx, actor.AccountID, id)
		if errors.Is(err, trainingErrors.ErrRoutineNotFound) {
			return reject("NOT_FOUND", "El borrador no está disponible.")
		}
		if err != nil {
			return err
		}
		fits := func(changes []protocol.Change) (bool, error) {
			err := d.Commands.CheckChanges(tx, actor.AccountID, op, changes)
			if errors.Is(err, protocol.ErrCommandLimit) {
				return false, nil
			}
			return err == nil, err
		}
		conflict := func(reason string) error {
			proposal := protocol.Conflict{ID: uuid.New(), AccountID: actor.AccountID, OperationID: op, EntityType: "routine_draft", EntityID: id, BaseRevision: base, CurrentRevision: current.Revision, Proposal: canonical, Reason: reason, CreatedAt: now}
			change, err := proposal.Change()
			if err != nil {
				return err
			}
			valid, err := fits([]protocol.Change{change})
			if err != nil {
				return err
			}
			if !valid {
				return reject("LIMIT_EXCEEDED", "La propuesta supera el tamaño permitido. Conserva la captura y reduce su contenido.")
			}
			if err = d.Commands.SaveConflict(tx, proposal); err != nil {
				return err
			}
			code, message := "REVISION_CONFLICT", "El borrador cambió en otro dispositivo. Conservamos tu propuesta."
			if reason == "deleted" {
				code = "ENTITY_DELETED"
				message = "La rutina se eliminó. Conservamos tu propuesta para revisarla."
			}
			result = protocol.Failure(op, "conflict", code, message, "review", false)
			result.ConflictID = proposal.ID.String()
			return finish([]protocol.Change{change})
		}
		if root.DeletedAt != nil {
			return conflict("deleted")
		}
		if root.Status == "archived" {
			return reject("REVISION_CONFLICT", "La rutina está archivada. Restáurala antes de editar.")
		}
		if current.Revision != base {
			return conflict("revision")
		}
		if current.Revision == math.MaxInt64 {
			return protocol.ErrCommandPersistence
		}
		available, err := d.Drafts.ReferencesAvailableForSave(tx, actor.AccountID, id, in.Payload.Content, baseVersionID)
		if err != nil {
			return err
		}
		if !available {
			return reject("VALIDATION_ERROR", "Revisa la versión de origen y los ejercicios del borrador.")
		}
		if err = current.Save(in.Payload.Content, baseVersionID, base, now); err != nil {
			return err
		}
		change, err := savedDraftChange(current)
		if err != nil {
			return err
		}
		valid, err := fits([]protocol.Change{change})
		if err != nil {
			return err
		}
		if !valid {
			return reject("LIMIT_EXCEEDED", "El borrador supera el tamaño permitido. Reduce su contenido.")
		}
		saved, err := d.Drafts.Save(tx, actor.AccountID, current, base)
		if errors.Is(err, trainingErrors.ErrRoutineReference) {
			return reject("VALIDATION_ERROR", "Revisa la versión de origen y los ejercicios del borrador.")
		}
		if errors.Is(err, trainingErrors.ErrTrainingLimit) {
			return reject("LIMIT_EXCEEDED", "El borrador supera el tamaño de almacenamiento permitido. Reduce su contenido.")
		}
		if err != nil {
			return err
		}
		if !saved {
			return trainingErrors.ErrRoutineRevisionConflict
		}
		result = protocol.OperationResult{OperationID: op.String(), Outcome: "accepted", Affected: []protocol.Affected{{EntityType: "routine_draft", EntityID: id.String(), Revision: strconv.FormatInt(current.Revision, 10)}}, CommittedAt: protocol.Timestamp(now), Result: &struct{}{}}
		return finish([]protocol.Change{change})
	})
	if err != nil {
		return protocol.OperationResult{}, err
	}
	return result, nil
}
func savedDraftChange(d *routine.DraftRecord) (protocol.Change, error) {
	var base *string
	if d.BaseVersionID != nil {
		value := d.BaseVersionID.String()
		base = &value
	}
	view := struct {
		ID            string         `json:"id"`
		Revision      string         `json:"revision"`
		BaseVersionID *string        `json:"base_version_id"`
		Content       *routine.Draft `json:"content"`
		DeletedAt     *string        `json:"deleted_at"`
	}{ID: d.RoutineID.String(), Revision: strconv.FormatInt(d.Revision, 10), BaseVersionID: base, Content: d.Content}
	raw, err := json.Marshal(view)
	return protocol.Change{EntityType: "routine_draft", EntityID: d.RoutineID.String(), Revision: strconv.FormatInt(d.Revision, 10), Data: raw}, err
}
