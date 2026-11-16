package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

type PublishRoutineDependencies struct {
	UoW           shared.UnitOfWork
	Routines      repository.RoutineRepository
	Publications  repository.RoutinePublicationRepository
	Commands      protocolRepository.Commands
	Canonicalizer protocol.Canonicalizer
	Now           func() time.Time
}
type PublishRoutine struct{ d PublishRoutineDependencies }

func NewPublishRoutine(d PublishRoutineDependencies) (*PublishRoutine, error) {
	if d.UoW == nil || d.Routines == nil || d.Publications == nil || d.Commands == nil || d.Canonicalizer == nil || d.Now == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &PublishRoutine{d}, nil
}
func (u *PublishRoutine) Execute(ctx context.Context, actor security.AccessClaims, in routine.PublishCommand) (protocol.OperationResult, error) {
	if !protocol.ValidID(actor.AccountID) || !protocol.ValidID(actor.DeviceID) || !protocol.ValidID(actor.SessionID) {
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
	draftRevision, _ := protocol.ParseRevision(in.Payload.DraftRevision)
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
		base, dependency, err := resolveTrainingBase(tx, d.Commands, actor.AccountID, op, in.Base, "routine", in.EntityID)
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
		if base < 1 || draftRevision < 1 {
			return reject("VALIDATION_ERROR", "Revisa las revisiones de la rutina y del borrador.")
		}
		root, err := d.Routines.GetByID(tx, actor.AccountID, id)
		if errors.Is(err, trainingErrors.ErrRoutineNotFound) {
			return reject("NOT_FOUND", "La rutina no está disponible.")
		}
		if err != nil {
			return err
		}
		draft, err := d.Routines.GetDraftByID(tx, actor.AccountID, id)
		if errors.Is(err, trainingErrors.ErrRoutineNotFound) {
			return reject("NOT_FOUND", "El borrador no está disponible.")
		}
		if err != nil {
			return err
		}
		conflict := func(entity, reason string, expected, current int64) error {
			proposal := protocol.Conflict{ID: uuid.New(), AccountID: actor.AccountID, OperationID: op, EntityType: entity, EntityID: id, BaseRevision: expected, CurrentRevision: current, Proposal: canonical, Reason: reason, CreatedAt: now}
			change, err := proposal.Change()
			if err != nil {
				return err
			}
			changes := []protocol.Change{change}
			if err = d.Commands.CheckChanges(tx, actor.AccountID, op, changes); errors.Is(err, protocol.ErrCommandLimit) {
				return reject("LIMIT_EXCEEDED", "La propuesta supera el tamaño permitido. Conserva la captura y reduce su contenido.")
			}
			if err != nil {
				return err
			}
			if err = d.Commands.SaveConflict(tx, proposal); err != nil {
				return err
			}
			code, message := "REVISION_CONFLICT", "La rutina o su borrador cambió. Conservamos tu propuesta."
			if reason == "deleted" {
				code = "ENTITY_DELETED"
				message = "La rutina o su borrador ya no está disponible. Conservamos tu propuesta."
			}
			result = protocol.Failure(op, "conflict", code, message, "review", false)
			result.ConflictID = proposal.ID.String()
			return finish(changes)
		}
		// Both compare-and-swap checks precede all exercise/material resolution.
		if root.DeletedAt != nil {
			return conflict("routine", "deleted", base, root.Revision)
		}
		if root.Status == "archived" {
			return reject("REVISION_CONFLICT", "La rutina está archivada. Restáurala antes de guardar una versión.")
		}
		if root.Revision != base {
			return conflict("routine", "revision", base, root.Revision)
		}
		if draft.Revision != draftRevision {
			return conflict("routine_draft", "revision", draftRevision, draft.Revision)
		}
		if draft.DeletedAt != nil || draft.Content == nil {
			return conflict("routine_draft", "deleted", draftRevision, draft.Revision)
		}
		material, err := d.Publications.ResolveMaterial(tx, actor.AccountID, draft)
		if errors.Is(err, trainingErrors.ErrRoutineReference) || errors.Is(err, trainingErrors.ErrInvalidRoutine) {
			return reject("VALIDATION_ERROR", "Completa el borrador y revisa sus ejercicios antes de guardar la rutina.")
		}
		if errors.Is(err, trainingErrors.ErrTrainingLimit) {
			return reject("LIMIT_EXCEEDED", "La rutina alcanzó un límite de publicación.")
		}
		if err != nil {
			return err
		}
		publication, err := routine.NewPublication(root, draft, in.Payload, material, actor.DeviceID, now, d.Canonicalizer)
		if errors.Is(err, trainingErrors.ErrTrainingLimit) || errors.Is(err, protocol.ErrRoutineVersionTooLarge) {
			return reject("LIMIT_EXCEEDED", "La versión supera el tamaño permitido. Reduce su contenido.")
		}
		if errors.Is(err, trainingErrors.ErrInvalidRoutine) || errors.Is(err, trainingErrors.ErrRoutineReference) || errors.Is(err, protocol.ErrRoutineVersionInvalid) {
			return reject("VALIDATION_ERROR", "Completa el borrador y revisa el mapa de días y ejercicios.")
		}
		if err != nil {
			return err
		}
		changes, err := publicationChanges(publication)
		if err != nil {
			return err
		}
		if err = d.Commands.CheckChanges(tx, actor.AccountID, op, changes); errors.Is(err, protocol.ErrCommandLimit) {
			return reject("LIMIT_EXCEEDED", "La publicación supera el tamaño permitido. Reduce su contenido.")
		}
		if err != nil {
			return err
		}
		created, err := d.Publications.Publish(tx, actor.AccountID, publication)
		if err != nil {
			return err
		}
		if !created {
			return reject("REVISION_CONFLICT", "No pudimos guardar la versión con esas identidades.")
		}
		result = protocol.OperationResult{OperationID: op.String(), Outcome: "accepted", CommittedAt: protocol.Timestamp(now), Result: &struct{}{}, Affected: []protocol.Affected{
			{EntityType: "routine", EntityID: id.String(), Revision: strconv.FormatInt(publication.Root.Revision, 10)},
			{EntityType: "routine_version", EntityID: publication.Version.ID.String(), Revision: "1"},
			{EntityType: "routine_draft", EntityID: id.String(), Revision: strconv.FormatInt(publication.Draft.Revision, 10)},
		}}
		return finish(changes)
	})
	if err != nil {
		return protocol.OperationResult{}, err
	}
	return result, nil
}

func publicationChanges(p *routine.Publication) ([]protocol.Change, error) {
	r := p.Root
	var duplicated *string
	if r.DuplicatedFromVersionID != nil {
		value := r.DuplicatedFromVersionID.String()
		duplicated = &value
	}
	root := struct {
		ID                      string  `json:"id"`
		Revision                string  `json:"revision"`
		CreatedAt               string  `json:"created_at"`
		UpdatedAt               string  `json:"updated_at"`
		DeletedAt               *string `json:"deleted_at"`
		Title                   string  `json:"title"`
		Status                  string  `json:"status"`
		CurrentVersionID        string  `json:"current_version_id"`
		DuplicatedFromVersionID *string `json:"duplicated_from_version_id"`
	}{ID: r.ID.String(), Revision: strconv.FormatInt(r.Revision, 10), CreatedAt: protocol.Timestamp(r.CreatedAt), UpdatedAt: protocol.Timestamp(r.UpdatedAt), Title: r.Title, Status: r.Status, CurrentVersionID: p.Version.ID.String(), DuplicatedFromVersionID: duplicated}
	draft := struct {
		ID            string         `json:"id"`
		Revision      string         `json:"revision"`
		BaseVersionID string         `json:"base_version_id"`
		Content       *routine.Draft `json:"content"`
		DeletedAt     string         `json:"deleted_at"`
	}{ID: p.Draft.RoutineID.String(), Revision: strconv.FormatInt(p.Draft.Revision, 10), BaseVersionID: p.Version.ID.String(), DeletedAt: protocol.Timestamp(*p.Draft.DeletedAt)}
	rootRaw, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	versionRaw, err := json.Marshal(p.Version.DTO())
	if err != nil {
		return nil, err
	}
	draftRaw, err := json.Marshal(draft)
	if err != nil {
		return nil, err
	}
	return []protocol.Change{{EntityType: "routine", EntityID: r.ID.String(), Revision: root.Revision, Data: rootRaw}, {EntityType: "routine_version", EntityID: p.Version.ID.String(), Revision: "1", Data: versionRaw}, {EntityType: "routine_draft", EntityID: r.ID.String(), Revision: draft.Revision, Data: draftRaw}}, nil
}
