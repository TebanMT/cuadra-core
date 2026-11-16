package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

type CreatePersonalExerciseDependencies struct {
	UoW           shared.UnitOfWork
	Exercises     repository.PersonalExerciseRepository
	Commands      protocolRepository.Commands
	Canonicalizer protocol.Canonicalizer
	Now           func() time.Time
}
type CreatePersonalExercise struct {
	dependencies CreatePersonalExerciseDependencies
}

func NewCreatePersonalExercise(d CreatePersonalExerciseDependencies) (*CreatePersonalExercise, error) {
	if d.UoW == nil || d.Exercises == nil || d.Commands == nil || d.Canonicalizer == nil || d.Now == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &CreatePersonalExercise{dependencies: d}, nil
}

func (u *CreatePersonalExercise) Execute(ctx context.Context, actor security.AccessClaims, in exercise.CreateCommand) (protocol.OperationResult, error) {
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
	d := u.dependencies
	canonical, err := d.Canonicalizer.Canonicalize(raw)
	if err != nil || len(canonical) > 1048576 {
		return protocol.OperationResult{}, protocol.ErrCommandInvalid
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
		if in.Base.Revision == nil || *in.Base.Revision != "0" || exercise.ValidateFields(in.Payload.Name, in.Payload.LoadMode, in.Payload.Notes) != nil {
			result = protocol.Failure(op, "rejected", "VALIDATION_ERROR", "Revisa el ejercicio y la revisión inicial.", "correct", false)
			return finish(nil)
		}
		created, err := exercise.NewPersonal(id, actor.AccountID, in.Payload.Name, in.Payload.LoadMode, in.Payload.Notes, now)
		if err != nil {
			return err
		}
		inserted, err := d.Exercises.Create(tx, actor.AccountID, created)
		if err != nil {
			return err
		}
		if !inserted {
			result = protocol.Failure(op, "rejected", "REVISION_CONFLICT", "No pudimos crear el ejercicio con esa identidad.", "review", false)
			return finish(nil)
		}
		view := struct {
			ID         string            `json:"id"`
			Revision   string            `json:"revision"`
			CreatedAt  string            `json:"created_at"`
			UpdatedAt  string            `json:"updated_at"`
			DeletedAt  *string           `json:"deleted_at"`
			Source     string            `json:"source"`
			Name       string            `json:"name"`
			LoadMode   exercise.LoadMode `json:"load_mode"`
			Notes      string            `json:"notes"`
			ArchivedAt *string           `json:"archived_at"`
		}{ID: id.String(), Revision: "1", CreatedAt: protocol.Timestamp(now), UpdatedAt: protocol.Timestamp(now), Source: "personal", Name: created.Name, LoadMode: created.LoadMode, Notes: created.Notes}
		projection, err := json.Marshal(view)
		if err != nil {
			return err
		}
		result = protocol.OperationResult{OperationID: op.String(), Outcome: "accepted", Affected: []protocol.Affected{{EntityType: "personal_exercise", EntityID: id.String(), Revision: "1"}}, CommittedAt: protocol.Timestamp(now), Result: &struct{}{}}
		return finish([]protocol.Change{{EntityType: "personal_exercise", EntityID: id.String(), Revision: "1", Data: projection}})
	})
	if err != nil {
		return protocol.OperationResult{}, err
	}
	return result, nil
}
