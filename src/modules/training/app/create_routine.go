package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

type CreateRoutineDependencies struct {
	UoW           shared.UnitOfWork
	Routines      repository.RoutineRepository
	Commands      protocolRepository.Commands
	Stream        protocolRepository.Stream
	Canonicalizer protocol.Canonicalizer
	Now           func() time.Time
}
type CreateRoutine struct{ dependencies CreateRoutineDependencies }

func NewCreateRoutine(d CreateRoutineDependencies) (*CreateRoutine, error) {
	if d.UoW == nil || d.Routines == nil || d.Commands == nil || d.Stream == nil || d.Canonicalizer == nil || d.Now == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &CreateRoutine{dependencies: d}, nil
}
func (u *CreateRoutine) Execute(ctx context.Context, actor security.AccessClaims, in routine.CreateCommand) (protocol.OperationResult, error) {
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
	d := u.dependencies
	canonical, err := d.Canonicalizer.Canonicalize(raw)
	if err != nil {
		return protocol.OperationResult{}, protocol.ErrCommandInvalid
	}
	if len(canonical) > 1048576 {
		return protocol.OperationResult{}, trainingErrors.ErrTrainingLimit
	}
	hash := sha256.Sum256(canonical)
	op, origin, id := uuid.MustParse(in.OperationID), uuid.MustParse(in.OriginDeviceID), uuid.MustParse(in.EntityID)
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
		if in.Base.Revision == nil || *in.Base.Revision != "0" {
			return reject("VALIDATION_ERROR", "Revisa la revisión inicial de la rutina.")
		}
		available, err := d.Routines.ReferencesAvailable(tx, actor.AccountID, in.Payload.Draft.References())
		if err != nil {
			return err
		}
		if !available {
			return reject("VALIDATION_ERROR", "Revisa los ejercicios de la rutina.")
		}
		r, draft, err := routine.NewDraftRoutine(id, actor.AccountID, in.Payload.Draft, now)
		if err != nil {
			return err
		}
		changes, err := routineCreationChanges(r, draft)
		if err != nil {
			return err
		}
		// Validate both canonical transport and PostgreSQL JSONB storage before
		// any business effect, so a size limit can receive a terminal receipt.
		if err = d.Commands.CheckChanges(tx, actor.AccountID, op, changes); errors.Is(err, protocol.ErrCommandLimit) {
			return reject("LIMIT_EXCEEDED", "La rutina supera el tamaño permitido. Reduce su contenido.")
		}
		if err != nil {
			return err
		}
		created, err := d.Routines.Create(tx, actor.AccountID, r, draft)
		if errors.Is(err, trainingErrors.ErrTrainingLimit) {
			return reject("LIMIT_EXCEEDED", "La rutina supera el tamaño de almacenamiento permitido. Reduce su contenido.")
		}
		if errors.Is(err, trainingErrors.ErrRoutineReference) {
			return reject("VALIDATION_ERROR", "Revisa los ejercicios de la rutina.")
		}
		if err != nil {
			return err
		}
		if !created {
			result = protocol.Failure(op, "rejected", "REVISION_CONFLICT", "No pudimos crear la rutina con esa identidad.", "review", false)
			return finish(nil)
		}
		result = protocol.OperationResult{OperationID: op.String(), Outcome: "accepted", Affected: []protocol.Affected{{EntityType: "routine", EntityID: id.String(), Revision: "1"}, {EntityType: "routine_draft", EntityID: id.String(), Revision: "1"}}, CommittedAt: protocol.Timestamp(now), Result: &struct{}{}}
		return finish(changes)
	})
	if err != nil {
		return protocol.OperationResult{}, err
	}
	return result, nil
}

func routineCreationChanges(r *routine.Routine, d *routine.DraftRecord) ([]protocol.Change, error) {
	root := struct {
		ID                      string  `json:"id"`
		Revision                string  `json:"revision"`
		CreatedAt               string  `json:"created_at"`
		UpdatedAt               string  `json:"updated_at"`
		DeletedAt               *string `json:"deleted_at"`
		Title                   string  `json:"title"`
		Status                  string  `json:"status"`
		CurrentVersionID        *string `json:"current_version_id"`
		DuplicatedFromVersionID *string `json:"duplicated_from_version_id"`
	}{ID: r.ID.String(), Revision: "1", CreatedAt: protocol.Timestamp(r.CreatedAt), UpdatedAt: protocol.Timestamp(r.UpdatedAt), Title: r.Title, Status: r.Status}
	edit := struct {
		ID            string         `json:"id"`
		Revision      string         `json:"revision"`
		BaseVersionID *string        `json:"base_version_id"`
		Content       *routine.Draft `json:"content"`
		DeletedAt     *string        `json:"deleted_at"`
	}{ID: d.RoutineID.String(), Revision: "1", Content: d.Content}
	rawRoot, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	rawDraft, err := json.Marshal(edit)
	if err != nil {
		return nil, err
	}
	return []protocol.Change{{EntityType: "routine", EntityID: r.ID.String(), Revision: "1", Data: rawRoot}, {EntityType: "routine_draft", EntityID: d.RoutineID.String(), Revision: "1", Data: rawDraft}}, nil
}
