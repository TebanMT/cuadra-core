package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

// These tests exercise orchestration only. Transaction/Start doubles are not
// PostgreSQL, publication, authorization or end-to-end workout evidence.
type pushTestTx struct{}

func (tx *pushTestTx) Execute(fn func(shared.Transaction) error) error { return fn(tx) }

type pushTestUoW struct {
	shared.UnitOfWork
	calls, commits int
	active         bool
	before         func()
	afterError     error
}

func (u *pushTestUoW) Command(_ context.Context, fn func(shared.Transaction) error) error {
	u.calls++
	if u.before != nil {
		u.before()
	}
	u.active = true
	err := fn(&pushTestTx{})
	u.active = false
	if err != nil {
		return err
	}
	if u.afterError != nil {
		return u.afterError
	}
	u.commits++
	return nil
}

type pushTestCommands struct {
	protocolRepository.Commands
	origins []uuid.UUID
	check   func(shared.Transaction, protocol.Actor, uuid.UUID) error
}

func (c *pushTestCommands) RequireActor(tx shared.Transaction, actor protocol.Actor, origin uuid.UUID) error {
	c.origins = append(c.origins, origin)
	if c.check != nil {
		return c.check(tx, actor, origin)
	}
	return nil
}

type pushTestStart struct {
	commands []workout.StartCommand
	run      func(context.Context, security.AccessClaims, workout.StartCommand) (protocol.OperationResult, error)
}

func (s *pushTestStart) Execute(ctx context.Context, actor security.AccessClaims, command workout.StartCommand) (protocol.OperationResult, error) {
	s.commands = append(s.commands, command)
	if s.run != nil {
		return s.run(ctx, actor, command)
	}
	return protocol.OperationResult{OperationID: command.OperationID, Outcome: "accepted"}, nil
}

type pushTestCodec func([]byte) ([]byte, error)

func (c pushTestCodec) Canonicalize(raw []byte) ([]byte, error) { return c(raw) }

func pushTestInput(n int) (security.AccessClaims, []workout.StartCommand) {
	actor := security.AccessClaims{AccountID: uuid.New(), DeviceID: uuid.New(), SessionID: uuid.New()}
	commands := make([]workout.StartCommand, n)
	for i := range commands {
		zero := "0"
		commands[i] = workout.StartCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), Type: workout.StartCommandType,
			EntityID: uuid.NewString(), Base: protocol.Base{Revision: &zero}, CapturedAt: "2026-10-19T00:00:00Z",
			Payload: workout.StartPayload{RoutineVersionID: uuid.NewString(), RoutineDayID: uuid.NewString(), VersionHash: strings.Repeat("a", 64),
				StartedAt: "2026-10-19T00:00:00.000Z", Timezone: "America/Mexico_City", Exercises: []workout.ExerciseMap{{SourceExerciseID: uuid.NewString(), ID: uuid.NewString()}}}}
	}
	return actor, commands
}

func pushTestDependencies() (PushWorkoutsDependencies, *pushTestUoW, *pushTestCommands, *pushTestStart) {
	uow, commands, start := &pushTestUoW{}, &pushTestCommands{}, &pushTestStart{}
	return PushWorkoutsDependencies{UoW: uow, Commands: commands, Start: start, CreateSet: &pushTestSet{}, Canonicalizer: crypto.JSONCanonicalizer{}}, uow, commands, start
}

func newTestPush(t *testing.T, d PushWorkoutsDependencies) *PushWorkouts {
	t.Helper()
	u, err := NewPushWorkouts(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPushWorkoutsRejectsMissingDependenciesAndInvalidWholeBatchBeforeIO(t *testing.T) {
	for _, missing := range []string{"uow", "commands", "start", "set", "codec"} {
		d, _, _, _ := pushTestDependencies()
		switch missing {
		case "uow":
			d.UoW = nil
		case "commands":
			d.Commands = nil
		case "start":
			d.Start = nil
		case "set":
			d.CreateSet = nil
		case "codec":
			d.Canonicalizer = nil
		}
		if _, err := NewPushWorkouts(d); !errors.Is(err, trainingErrors.ErrTrainingConfiguration) {
			t.Fatal("missing dependency accepted", missing)
		}
	}
	for _, invalid := range []string{"empty", "too-many", "duplicate", "last-invalid", "codec-error", "command-size"} {
		t.Run(invalid, func(t *testing.T) {
			d, uow, commands, start := pushTestDependencies()
			actor, input := pushTestInput(2)
			expected := protocol.ErrCommandInvalid
			switch invalid {
			case "empty":
				input = nil
			case "too-many":
				actor, input = pushTestInput(51)
			case "duplicate":
				input[1] = input[0]
			case "last-invalid":
				input[1].CapturedAt = "2026-02-30T00:00:00Z"
			case "codec-error":
				d.Canonicalizer = pushTestCodec(func([]byte) ([]byte, error) { return nil, errors.New("synthetic invalid JSON") })
			case "command-size":
				calls := 0
				d.Canonicalizer = pushTestCodec(func(raw []byte) ([]byte, error) {
					calls++
					if calls == 2 {
						return bytes.Repeat([]byte{'x'}, (1<<20)+1), nil
					}
					return (crypto.JSONCanonicalizer{}).Canonicalize(raw)
				})
				expected = protocol.ErrCommandLimit
			}
			out, err := newTestPush(t, d).Execute(context.Background(), actor, startOperations(input))
			if !errors.Is(err, expected) || out != nil || uow.calls != 0 || len(commands.origins) != 0 || len(start.commands) != 0 {
				t.Fatal("invalid batch reached I/O", err)
			}
		})
	}
}

func TestPushWorkoutsRejectsActorMixedScopesAndPreflightFailuresWithoutStarting(t *testing.T) {
	actor, input := pushTestInput(2)
	for _, ctx := range []context.Context{shared.WithPersonalAccount(context.Background(), uuid.New()), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithTenant(context.Background(), uuid.New())} {
		d, uow, _, start := pushTestDependencies()
		out, err := newTestPush(t, d).Execute(ctx, actor, startOperations(input))
		if !errors.Is(err, shared.ErrPersonalScope) || out != nil || uow.calls != 0 || len(start.commands) != 0 {
			t.Fatal("mixed scope reached I/O", err)
		}
	}
	d, uow, _, start := pushTestDependencies()
	invalidActor := actor
	invalidActor.SessionID = uuid.Nil
	if _, err := newTestPush(t, d).Execute(context.Background(), invalidActor, startOperations(input)); !errors.Is(err, protocol.ErrCommandActor) || uow.calls != 0 {
		t.Fatal("invalid actor reached I/O", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newTestPush(t, d).Execute(ctx, actor, startOperations(input)); !errors.Is(err, context.Canceled) || uow.calls != 0 || len(start.commands) != 0 {
		t.Fatal("cancelled batch reached I/O", err)
	}
	for _, failure := range []error{protocol.ErrCommandOrigin, protocol.ErrCommandActor, protocol.ErrCommandPersistence} {
		d, uow, commands, start := pushTestDependencies()
		input[1].OriginDeviceID = uuid.NewString()
		commands.check = func(shared.Transaction, protocol.Actor, uuid.UUID) error {
			if len(commands.origins) == 2 {
				return failure
			}
			return nil
		}
		out, err := newTestPush(t, d).Execute(context.Background(), actor, startOperations(input))
		if !errors.Is(err, failure) || out != nil || uow.commits != 0 || len(start.commands) != 0 {
			t.Fatal("preflight failure started operations", err)
		}
	}
	d, uow, _, start = pushTestDependencies()
	uow.afterError = context.DeadlineExceeded
	if _, err := newTestPush(t, d).Execute(context.Background(), actor, startOperations(input)); !errors.Is(err, context.DeadlineExceeded) || len(start.commands) != 0 {
		t.Fatal("failed preflight commit started operations", err)
	}
}

func TestPushWorkoutsPreflightsUniqueOriginsThenRunsDetachedCommandsInOrder(t *testing.T) {
	d, uow, commands, start := pushTestDependencies()
	actor, input := pushTestInput(50)
	origin := uuid.New()
	input[20].OriginDeviceID, input[49].OriginDeviceID = origin.String(), origin.String()
	after := uuid.NewString()
	input[2].Base = protocol.Base{AfterOperationID: &after}
	raw, _ := json.Marshal(input)
	var original []workout.StartCommand
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	uow.before = func() {
		*input[0].Base.Revision = "12"
		input[1].Payload.Exercises[0].ID = uuid.NewString()
		*input[2].Base.AfterOperationID = uuid.NewString()
		input[3].CapturedAt = "2026-10-19T00:00:00.000Z"
	}
	commands.check = func(tx shared.Transaction, a protocol.Actor, _ uuid.UUID) error {
		if _, ok := tx.(*pushTestTx); !ok || !uow.active || a.AccountID != actor.AccountID || a.DeviceID != actor.DeviceID || a.SessionID != actor.SessionID {
			t.Fatal("preflight lost transaction or actor binding")
		}
		return nil
	}
	start.run = func(ctx context.Context, a security.AccessClaims, command workout.StartCommand) (protocol.OperationResult, error) {
		if uow.active || uow.commits != 1 || a != actor {
			t.Fatal("operation ran in preflight or wrong actor")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("batch deadline missing")
		}
		return protocol.OperationResult{OperationID: command.OperationID, Outcome: "accepted"}, nil
	}
	results, err := newTestPush(t, d).Execute(context.Background(), actor, startOperations(input))
	if err != nil || len(results) != 50 || !reflect.DeepEqual(start.commands, original) || !reflect.DeepEqual(commands.origins, []uuid.UUID{actor.DeviceID, origin}) {
		t.Fatal("batch order, exact command capture or unique origin preflight changed", err)
	}
	for i, result := range results {
		if result.OperationID != original[i].OperationID || result.Outcome != "accepted" {
			t.Fatal("result order changed")
		}
	}
}

func TestPushWorkoutsPreservesResultsAndMapsIndividualFailuresWithoutLeakingErrors(t *testing.T) {
	d, _, _, start := pushTestDependencies()
	actor, input := pushTestInput(5)
	accepted := protocol.OperationResult{OperationID: input[0].OperationID, Outcome: "accepted", CommittedAt: "2026-10-19T01:00:00.000Z",
		Affected: []protocol.Affected{{EntityType: "workout", EntityID: input[0].EntityID, Revision: "1"}}, Result: &struct{}{}}
	business := protocol.Failure(uuid.MustParse(input[3].OperationID), "rejected", "VALIDATION_ERROR", "Revisa el entrenamiento.", "correct", false)
	start.run = func(_ context.Context, _ security.AccessClaims, command workout.StartCommand) (protocol.OperationResult, error) {
		switch command.OperationID {
		case input[0].OperationID:
			return accepted, nil
		case input[1].OperationID:
			return protocol.OperationResult{}, errors.New("secret-raw-database-body")
		case input[2].OperationID:
			return protocol.OperationResult{OperationID: uuid.NewString(), Outcome: "accepted"}, protocol.ErrOperationMismatch
		case input[3].OperationID:
			return business, nil
		default:
			return protocol.OperationResult{OperationID: uuid.NewString(), Outcome: "accepted"}, nil
		}
	}
	results, err := newTestPush(t, d).Execute(context.Background(), actor, startOperations(input))
	if err != nil || len(start.commands) != 5 || len(results) != 5 || !reflect.DeepEqual(results[0], accepted) || !reflect.DeepEqual(results[3], business) {
		t.Fatal("individual outcome lost or independent operation skipped", err)
	}
	for i, code := range map[int]string{1: "TEMPORARY_UNAVAILABLE", 2: "IDEMPOTENCY_MISMATCH", 4: "TEMPORARY_UNAVAILABLE"} {
		if results[i].OperationID != input[i].OperationID || results[i].Error == nil || results[i].Error.Code != code {
			t.Fatal("individual failure mismatch")
		}
	}
	if results[2].Outcome != "rejected" || results[2].Error.Retryable || results[2].Error.Action != "review" {
		t.Fatal("idempotency mismatch became retry or disclosed original receipt")
	}
	raw, _ := json.Marshal(results)
	if bytes.Contains(raw, []byte("secret-raw-database-body")) {
		t.Fatal("raw failure escaped")
	}
}

func TestPushWorkoutsActorRevocationStopsRemainderAndKeepsEarlierCommit(t *testing.T) {
	d, _, _, start := pushTestDependencies()
	actor, input := pushTestInput(4)
	start.run = func(_ context.Context, _ security.AccessClaims, command workout.StartCommand) (protocol.OperationResult, error) {
		if command.OperationID == input[0].OperationID {
			return protocol.OperationResult{OperationID: command.OperationID, Outcome: "accepted"}, nil
		}
		return protocol.OperationResult{}, protocol.ErrCommandActor
	}
	results, err := newTestPush(t, d).Execute(context.Background(), actor, startOperations(input))
	if err != nil || len(start.commands) != 2 || len(results) != 4 || results[0].Outcome != "accepted" {
		t.Fatal("revocation lost prior result or ran withdrawn actor", err)
	}
	for i := 1; i < len(results); i++ {
		r := results[i]
		if r.OperationID != input[i].OperationID || r.Outcome != "retry" || r.Error == nil || r.Error.Code != "SESSION_REVOKED" || r.Error.Action != "authenticate" || r.Error.Retryable {
			t.Fatal("withdrawn actor result permits automatic retry")
		}
	}
}

func TestPushWorkoutsDeadlineAndCancellationFillEveryUnstartedResult(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel-after-accepted"} {
		t.Run(mode, func(t *testing.T) {
			d, _, _, start := pushTestDependencies()
			actor, input := pushTestInput(3)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			u := newTestPush(t, d)
			u.timeout = 200 * time.Millisecond
			start.run = func(ctx context.Context, _ security.AccessClaims, command workout.StartCommand) (protocol.OperationResult, error) {
				if mode == "cancel-after-accepted" {
					cancel()
					return protocol.OperationResult{OperationID: command.OperationID, Outcome: "accepted"}, nil
				}
				<-ctx.Done()
				return protocol.OperationResult{}, ctx.Err()
			}
			results, err := u.Execute(ctx, actor, startOperations(input))
			if err != nil || len(start.commands) != 1 || len(results) != 3 {
				t.Fatal("cut batch omitted results or started more work", err)
			}
			for i, result := range results {
				if mode == "cancel-after-accepted" && i == 0 {
					if result.Outcome != "accepted" {
						t.Fatal("cancel discarded acknowledged effect")
					}
					continue
				}
				if result.OperationID != input[i].OperationID || result.Outcome != "retry" || result.Error == nil || result.Error.Code != "TEMPORARY_UNAVAILABLE" || !result.Error.Retryable {
					t.Fatal("missing bounded retry result")
				}
			}
		})
	}
}

type pushTestSet struct {
	run func(context.Context, security.AccessClaims, workout.CreateSetCommand) (protocol.OperationResult, error)
}

func (s *pushTestSet) Execute(ctx context.Context, actor security.AccessClaims, c workout.CreateSetCommand) (protocol.OperationResult, error) {
	if s.run != nil {
		return s.run(ctx, actor, c)
	}
	return protocol.OperationResult{OperationID: c.OperationID, Outcome: "accepted"}, nil
}
func startOperations(commands []workout.StartCommand) []workout.OfflineCommand {
	out := make([]workout.OfflineCommand, len(commands))
	for i := range commands {
		out[i].Start = &commands[i]
	}
	return out
}

func TestPushWorkoutsMixedUnionPreflightsSetAndPreservesOrderAndOriginalBody(t *testing.T) {
	d, uow, commands, start := pushTestDependencies()
	actor, starts := pushTestInput(1)
	zero, value, unit := "0", "30.0", "kg"
	set := workout.CreateSetCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), EntityID: uuid.NewString(), Type: workout.CreateSetCommandType, Base: protocol.Base{Revision: &zero}, CapturedAt: "2099-10-19T00:00:00Z", Payload: workout.CreateSetPayload{WorkoutID: starts[0].EntityID, WorkoutExerciseID: starts[0].Payload.Exercises[0].ID, ObservedWorkout: protocol.Base{AfterOperationID: &starts[0].OperationID}, Load: workout.LoadInput{LoadMode: "external", WeightValue: &value, WeightUnit: &unit}, Repetitions: 0}}
	input := []workout.OfflineCommand{{Start: &starts[0]}, {CreateSet: &set}}
	invalid := set
	invalid.OperationID = starts[0].OperationID
	if _, err := newTestPush(t, d).Execute(context.Background(), actor, []workout.OfflineCommand{input[0], {CreateSet: &invalid}}); !errors.Is(err, protocol.ErrCommandInvalid) || uow.calls != 0 {
		t.Fatal("cross-type duplicate reached I/O")
	}
	if _, err := newTestPush(t, d).Execute(context.Background(), actor, []workout.OfflineCommand{input[0], {}}); !errors.Is(err, protocol.ErrCommandInvalid) || uow.calls != 0 {
		t.Fatal("empty union reached I/O")
	}
	raw, _ := json.Marshal(set)
	var original workout.CreateSetCommand
	if json.Unmarshal(raw, &original) != nil {
		t.Fatal("bad fixture")
	}
	uow.before = func() { value = "99.0"; set.CapturedAt = "2099-10-19T00:00:00.000Z" }
	var order []string
	start.run = func(_ context.Context, _ security.AccessClaims, c workout.StartCommand) (protocol.OperationResult, error) {
		order = append(order, c.OperationID)
		return protocol.OperationResult{OperationID: c.OperationID, Outcome: "accepted"}, nil
	}
	d.CreateSet = &pushTestSet{run: func(_ context.Context, _ security.AccessClaims, c workout.CreateSetCommand) (protocol.OperationResult, error) {
		if !reflect.DeepEqual(c, original) || uow.active || len(commands.origins) != 1 {
			t.Fatal("set input mutated or missed common preflight")
		}
		order = append(order, c.OperationID)
		return protocol.OperationResult{OperationID: c.OperationID, Outcome: "accepted_with_review"}, nil
	}}
	out, err := newTestPush(t, d).Execute(context.Background(), actor, input)
	if err != nil || len(out) != 2 || out[1].Outcome != "accepted_with_review" || !reflect.DeepEqual(order, []string{starts[0].OperationID, set.OperationID}) {
		t.Fatal("mixed dispatch/order changed", err)
	}
}
