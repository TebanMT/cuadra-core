package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
	"time"
)

type StartWorkoutDependencies struct {
	UoW           shared.UnitOfWork
	Versions      repository.RoutineVersionReader
	Workouts      repository.WorkoutRepository
	Commands      protocolRepository.Commands
	Canonicalizer protocol.Canonicalizer
	Timezones     repository.TimezoneValidator
	Now           func() time.Time
}
type StartWorkout struct{ dependencies StartWorkoutDependencies }

func NewStartWorkout(d StartWorkoutDependencies) (*StartWorkout, error) {
	if d.UoW == nil || d.Versions == nil || d.Workouts == nil || d.Commands == nil || d.Canonicalizer == nil || d.Timezones == nil || d.Now == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &StartWorkout{d}, nil
}
func (u *StartWorkout) Execute(ctx context.Context, actor security.AccessClaims, in workout.StartCommand) (protocol.OperationResult, error) {
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
		if err := d.Commands.RequireActor(tx, protocol.Actor{AccountID: actor.AccountID, DeviceID: actor.DeviceID, SessionID: actor.SessionID}, origin); err != nil {
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
		if in.Base.Revision == nil || *in.Base.Revision != "0" || !d.Timezones.Valid(in.Payload.Timezone) {
			return reject("VALIDATION_ERROR", "Revisa el inicio y la zona horaria del entrenamiento.")
		}
		version, err := d.Versions.GetVersion(tx, actor.AccountID, uuid.MustParse(in.Payload.RoutineVersionID))
		if errors.Is(err, trainingErrors.ErrRoutineNotFound) {
			return reject("VALIDATION_ERROR", "La versión o el día del entrenamiento no están disponibles.")
		}
		if err != nil {
			return err
		}
		created, err := workout.NewStarted(id, actor.AccountID, origin, in.Payload, version, now)
		if errors.Is(err, trainingErrors.ErrInvalidWorkout) {
			return reject("VALIDATION_ERROR", "Revisa la versión, el día y los ejercicios del entrenamiento.")
		}
		if err != nil {
			return err
		}
		projection, err := json.Marshal(created.DTO())
		if err != nil {
			return err
		}
		changes := []protocol.Change{{EntityType: "workout", EntityID: id.String(), Revision: "1", Data: projection}}
		if err = d.Commands.CheckChanges(tx, actor.AccountID, op, changes); errors.Is(err, protocol.ErrCommandLimit) {
			return reject("LIMIT_EXCEEDED", "El entrenamiento supera el tamaño permitido.")
		}
		if err != nil {
			return err
		}
		inserted, err := d.Workouts.Create(tx, actor.AccountID, created)
		if err != nil {
			return err
		}
		if !inserted {
			return reject("REVISION_CONFLICT", "No pudimos crear el entrenamiento con esas identidades.")
		}
		result = protocol.OperationResult{OperationID: op.String(), Outcome: "accepted", Affected: []protocol.Affected{{EntityType: "workout", EntityID: id.String(), Revision: "1"}}, CommittedAt: protocol.Timestamp(now), Result: &struct{}{}}
		return finish(changes)
	})
	if err != nil {
		return protocol.OperationResult{}, err
	}
	return result, nil
}
