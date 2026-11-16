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
	"strconv"
	"time"
)

type CreateWorkoutSetDependencies struct {
	UoW           shared.UnitOfWork
	Workouts      repository.WorkoutRepository
	Sets          repository.WorkoutSetRepository
	Commands      protocolRepository.Commands
	Canonicalizer protocol.Canonicalizer
	Now           func() time.Time
}
type CreateWorkoutSet struct{ d CreateWorkoutSetDependencies }

func NewCreateWorkoutSet(d CreateWorkoutSetDependencies) (*CreateWorkoutSet, error) {
	if d.UoW == nil || d.Workouts == nil || d.Sets == nil || d.Commands == nil || d.Canonicalizer == nil || d.Now == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &CreateWorkoutSet{d}, nil
}
func (u *CreateWorkoutSet) Execute(ctx context.Context, actor security.AccessClaims, in workout.CreateSetCommand) (protocol.OperationResult, error) {
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
	if err != nil || len(canonical) > 1048576 {
		return protocol.OperationResult{}, protocol.ErrCommandInvalid
	}
	hash := sha256.Sum256(canonical)
	op, id, origin, workoutID, appearance := uuid.MustParse(in.OperationID), uuid.MustParse(in.EntityID), uuid.MustParse(in.OriginDeviceID), uuid.MustParse(in.Payload.WorkoutID), uuid.MustParse(in.Payload.WorkoutExerciseID)
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
			return reject("VALIDATION_ERROR", "Revisa la revisión inicial de la serie.")
		}
		observed, dependency, err := resolveTrainingBase(tx, d.Commands, actor.AccountID, op, in.Payload.ObservedWorkout, "workout", in.Payload.WorkoutID)
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
		current, err := d.Workouts.GetByID(tx, actor.AccountID, workoutID)
		if errors.Is(err, trainingErrors.ErrWorkoutNotFound) {
			return reject("NOT_FOUND", "El entrenamiento no está disponible.")
		}
		if err != nil {
			return err
		}
		if observed < 1 || observed > current.Revision {
			return reject("VALIDATION_ERROR", "Revisa la revisión observada del entrenamiento.")
		}
		load, err := workout.CanonicalLoad(in.Payload.Load.LoadMode, in.Payload.Load.WeightValue, in.Payload.Load.WeightUnit)
		if err != nil {
			return reject("VALIDATION_ERROR", "Revisa la carga de la serie.")
		}
		captured, _ := protocol.ParseCapturedAt(in.CapturedAt)
		series, err := workout.NewSet(current, id, origin, appearance, in.Payload.PositionHint, in.Payload.Repetitions, load, captured, now)
		if err != nil {
			return reject("VALIDATION_ERROR", "Revisa el ejercicio y la carga de la serie.")
		}
		fits := func(changes []protocol.Change) (bool, error) {
			err := d.Commands.CheckChanges(tx, actor.AccountID, op, changes)
			if errors.Is(err, protocol.ErrCommandLimit) {
				return false, nil
			}
			return err == nil, err
		}
		makeConflict := func(reason string, revision int64) (protocol.Conflict, protocol.Change, error) {
			c := protocol.Conflict{ID: uuid.New(), AccountID: actor.AccountID, OperationID: op, EntityType: "workout", EntityID: workoutID, BaseRevision: observed, CurrentRevision: revision, Proposal: canonical, Reason: reason, CreatedAt: now}
			change, err := c.Change()
			return c, change, err
		}
		if current.DeletedAt != nil {
			c, change, err := makeConflict("deleted", current.Revision)
			if err != nil {
				return err
			}
			ok, err := fits([]protocol.Change{change})
			if err != nil {
				return err
			}
			if !ok {
				return reject("LIMIT_EXCEEDED", "La propuesta supera el tamaño permitido.")
			}
			if err = d.Commands.SaveConflict(tx, c); err != nil {
				return err
			}
			result = protocol.Failure(op, "conflict", "ENTITY_DELETED", "El entrenamiento se eliminó. Conservamos la serie propuesta.", "review", false)
			result.ConflictID = c.ID.String()
			return finish([]protocol.Change{change})
		}
		count, err := d.Sets.CountIDs(tx, actor.AccountID, workoutID)
		if err != nil {
			return err
		}
		if count >= workout.MaxSetIDs {
			return reject("LIMIT_EXCEEDED", "El entrenamiento alcanzó el límite de series.")
		}
		next, err := current.WithNewSet(series, now)
		if errors.Is(err, trainingErrors.ErrTrainingLimit) {
			return reject("LIMIT_EXCEEDED", "El entrenamiento alcanzó el límite de revisiones.")
		}
		if err != nil {
			return err
		}
		setData, err := json.Marshal(series.DTO())
		if err != nil {
			return err
		}
		rootData, err := json.Marshal(next.DTO())
		if err != nil {
			return err
		}
		revision := strconv.FormatInt(next.Revision, 10)
		changes := []protocol.Change{{EntityType: "workout_set", EntityID: id.String(), Revision: "1", Data: setData}, {EntityType: "workout", EntityID: workoutID.String(), Revision: revision, Data: rootData}}
		affected := []protocol.Affected{{EntityType: "workout_set", EntityID: id.String(), Revision: "1"}, {EntityType: "workout", EntityID: workoutID.String(), Revision: revision}}
		var conflict *protocol.Conflict
		if current.State == "completed" {
			c, change, err := makeConflict("finalization", next.Revision)
			if err != nil {
				return err
			}
			conflict = &c
			changes = append(changes, change)
			affected = append(affected, protocol.Affected{EntityType: "conflict", EntityID: c.ID.String(), Revision: "1"})
		}
		ok, err := fits(changes)
		if err != nil {
			return err
		}
		if !ok {
			return reject("LIMIT_EXCEEDED", "La serie supera el tamaño permitido.")
		}
		created, err := d.Sets.CreateAndAdvance(tx, actor.AccountID, series, next, current.Revision)
		if err != nil {
			return err
		}
		if !created {
			return reject("REVISION_CONFLICT", "No pudimos crear la serie con esa identidad.")
		}
		if conflict != nil {
			if err = d.Commands.SaveConflict(tx, *conflict); err != nil {
				return err
			}
		}
		result = protocol.OperationResult{OperationID: op.String(), Outcome: "accepted", Affected: affected, CommittedAt: protocol.Timestamp(now), Result: &struct{}{}}
		if conflict != nil {
			result.Outcome = "accepted_with_review"
			result.ConflictID = conflict.ID.String()
		}
		return finish(changes)
	})
	if err != nil {
		return protocol.OperationResult{}, err
	}
	return result, nil
}
