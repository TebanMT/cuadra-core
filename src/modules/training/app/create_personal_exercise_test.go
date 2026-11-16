package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

type forbiddenTrainingUoW struct {
	shared.UnitOfWork
	calls int
}

func (u *forbiddenTrainingUoW) Command(context.Context, func(shared.Transaction) error) error {
	u.calls++
	return errors.New("unexpected I/O")
}

type unusedExercises struct {
	repository.PersonalExerciseRepository
}
type unusedCommands struct{ protocolRepository.Commands }

func TestCreatePersonalExerciseRejectsInvalidScopeAndSchemaBeforeIO(t *testing.T) {
	uow := &forbiddenTrainingUoW{}
	d := CreatePersonalExerciseDependencies{UoW: uow, Exercises: unusedExercises{}, Commands: unusedCommands{}, Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now}
	u, err := NewCreatePersonalExercise(d)
	if err != nil {
		t.Fatal(err)
	}
	a := security.AccessClaims{AccountID: uuid.New(), DeviceID: uuid.New(), SessionID: uuid.New()}
	zero := "0"
	c := exercise.CreateCommand{OperationID: uuid.NewString(), OriginDeviceID: a.DeviceID.String(), Type: exercise.CreateCommandType, EntityID: uuid.NewString(), Base: protocol.Base{Revision: &zero}, CapturedAt: "2026-10-19T00:00:00Z", Payload: exercise.CreatePayload{Name: "Press", LoadMode: exercise.External, Notes: ""}}
	for _, ctx := range []context.Context{shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithPersonalAccount(context.Background(), uuid.New()), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err := u.Execute(ctx, a, c); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("mixed scope reached persistence", err)
		}
	}
	bad := a
	bad.DeviceID = uuid.Nil
	if _, err := u.Execute(context.Background(), bad, c); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("invalid actor accepted", err)
	}
	for _, edit := range []func(*exercise.CreateCommand){
		func(c *exercise.CreateCommand) { c.Payload.LoadMode = "unknown" },
		func(c *exercise.CreateCommand) { c.Payload.Name = string([]byte{0xff}) },
		func(c *exercise.CreateCommand) { c.Payload.Notes = "a\x00b" },
		func(c *exercise.CreateCommand) { c.CapturedAt = "2026-02-30T00:00:00Z" },
		func(c *exercise.CreateCommand) { c.Type = "workout.start" },
	} {
		invalid := c
		edit(&invalid)
		if _, err := u.Execute(context.Background(), a, invalid); !errors.Is(err, protocol.ErrCommandInvalid) {
			t.Fatal("invalid schema reached persistence", err)
		}
	}
	if uow.calls != 0 {
		t.Fatal("guard attempted database I/O")
	}
	d.Exercises = nil
	if _, err := NewCreatePersonalExercise(d); !errors.Is(err, trainingErrors.ErrTrainingConfiguration) {
		t.Fatal("missing repository accepted")
	}
}
