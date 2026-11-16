package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

// StartWorkoutExecutor is deliberately narrow. Other offline commands need an
// explicit typed implementation before the batch can accept them.
type StartWorkoutExecutor interface {
	Execute(context.Context, security.AccessClaims, workout.StartCommand) (protocol.OperationResult, error)
}

type CreateWorkoutSetExecutor interface {
	Execute(context.Context, security.AccessClaims, workout.CreateSetCommand) (protocol.OperationResult, error)
}

type PushWorkoutsDependencies struct {
	UoW           shared.UnitOfWork
	Commands      protocolRepository.Commands
	Start         StartWorkoutExecutor
	CreateSet     CreateWorkoutSetExecutor
	Canonicalizer protocol.Canonicalizer
}

type PushWorkouts struct {
	dependencies PushWorkoutsDependencies
	timeout      time.Duration // Fixed in production; package tests can shorten the deadline.
}

func NewPushWorkouts(d PushWorkoutsDependencies) (*PushWorkouts, error) {
	if d.UoW == nil || d.Commands == nil || d.Start == nil || d.CreateSet == nil || d.Canonicalizer == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &PushWorkouts{dependencies: d, timeout: 30 * time.Second}, nil
}

// The preflight is a separate, short transaction. Each producer retains
// its own transaction, actor recheck and receipt; a batch is never atomic.
// HTTP owns the total request byte limit and the syntactic batch header UUID.
func (u *PushWorkouts) Execute(ctx context.Context, actor security.AccessClaims, input []workout.OfflineCommand) ([]protocol.OperationResult, error) {
	if !protocol.ValidID(actor.AccountID) || !protocol.ValidID(actor.DeviceID) || !protocol.ValidID(actor.SessionID) {
		return nil, protocol.ErrCommandActor
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return nil, err
	}
	if len(input) < 1 || len(input) > 50 {
		return nil, protocol.ErrCommandInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, u.timeout)
	defer cancel()
	d := u.dependencies
	commands := make([]workout.OfflineCommand, len(input))
	seen, originsSeen := map[string]bool{}, map[uuid.UUID]bool{}
	origins := []uuid.UUID{}
	for i, command := range input {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if command.ValidateEnvelope() != nil || seen[command.OperationID()] {
			return nil, protocol.ErrCommandInvalid
		}
		seen[command.OperationID()] = true
		raw, err := json.Marshal(command)
		if err != nil {
			return nil, protocol.ErrCommandInvalid
		}
		canonical, err := d.Canonicalizer.Canonicalize(raw)
		if err != nil {
			return nil, protocol.ErrCommandInvalid
		}
		if len(canonical) > 1<<20 {
			return nil, protocol.ErrCommandLimit
		}
		// Detach Base pointers and exercise slices before the first I/O without
		// changing any original string or turning an after-base into a revision.
		if json.Unmarshal(raw, &commands[i]) != nil {
			return nil, protocol.ErrCommandInvalid
		}
		origin := uuid.MustParse(command.OriginDeviceID())
		if !originsSeen[origin] {
			origins = append(origins, origin)
			originsSeen[origin] = true
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		for _, origin := range origins {
			if err := d.Commands.RequireActor(tx, protocol.Actor{AccountID: actor.AccountID, DeviceID: actor.DeviceID, SessionID: actor.SessionID}, origin); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	results := make([]protocol.OperationResult, 0, len(commands))
	actorUnavailable := false
	for _, command := range commands {
		op := uuid.MustParse(command.OperationID())
		if actorUnavailable {
			results = append(results, pushActorUnavailable(op))
			continue
		}
		if ctx.Err() != nil {
			results = append(results, pushTemporary(op))
			continue
		}
		var result protocol.OperationResult
		var err error
		if command.Start != nil {
			result, err = d.Start.Execute(ctx, actor, *command.Start)
		} else {
			result, err = d.CreateSet.Execute(ctx, actor, *command.CreateSet)
		}
		if err == nil && result.OperationID != command.OperationID() {
			err = protocol.ErrCommandPersistence
		}
		switch {
		case err == nil:
			results = append(results, result)
		case errors.Is(err, protocol.ErrOperationMismatch):
			// The existing receipt remains untouched and is never disclosed.
			results = append(results, protocol.Failure(op, "rejected", "IDEMPOTENCY_MISMATCH", "La operación cambió. Conserva el cambio original para revisarlo.", "review", false))
		case errors.Is(err, protocol.ErrCommandActor):
			actorUnavailable = true
			results = append(results, pushActorUnavailable(op))
		default:
			results = append(results, pushTemporary(op))
		}
	}
	return results, nil
}

func pushActorUnavailable(op uuid.UUID) protocol.OperationResult {
	return protocol.Failure(op, "retry", "SESSION_REVOKED", "La sesión ya no está disponible. Conservamos la operación para autenticarte de nuevo.", "authenticate", false)
}

func pushTemporary(op uuid.UUID) protocol.OperationResult {
	return protocol.Failure(op, "retry", "TEMPORARY_UNAVAILABLE", "No pudimos confirmar la operación. Conserva su contenido para reintentar.", "retry", true)
}
