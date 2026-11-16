//go:build server

package controllers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Wire-decoding regression only; the PostgreSQL suite proves real effects.
type pushWireExecutor struct {
	calls    int
	commands []workout.OfflineCommand
}

func (e *pushWireExecutor) Execute(_ context.Context, _ security.AccessClaims, commands []workout.OfflineCommand) ([]protocol.OperationResult, error) {
	e.calls++
	e.commands = commands
	out := make([]protocol.OperationResult, len(commands))
	for i, c := range commands {
		out[i] = protocol.OperationResult{OperationID: c.OperationID(), Outcome: "accepted"}
	}
	return out, nil
}

func TestPushHTTPClosedUnionDecodesWireDiscriminatorBeforeDispatch(t *testing.T) {
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := crypto.NewAccessTokens("wire_test", private, map[string]ed25519.PublicKey{"wire_test": pub})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	token, claims, err := tokens.Issue(uuid.New(), uuid.New(), uuid.New(), now)
	if err != nil {
		t.Fatal(err)
	}
	authenticate, err := personalMiddleware.NewPersonal(tokens, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	executor := &pushWireExecutor{}
	h, err := NewPushController(executor)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/mancuerna/v1/sync/push", authenticate, h.Execute)
	zero, value, unit := "0", "30.0", "kg"
	start := workout.StartCommand{OperationID: uuid.NewString(), OriginDeviceID: claims.DeviceID.String(), EntityID: uuid.NewString(), Type: workout.StartCommandType, Base: protocol.Base{Revision: &zero}, CapturedAt: "2026-10-19T00:00:00Z", Payload: workout.StartPayload{RoutineVersionID: uuid.NewString(), RoutineDayID: uuid.NewString(), VersionHash: strings.Repeat("a", 64), StartedAt: "2026-10-19T00:00:00.000Z", Timezone: "America/Mexico_City", Exercises: []workout.ExerciseMap{{SourceExerciseID: uuid.NewString(), ID: uuid.NewString()}}}}
	set := workout.CreateSetCommand{OperationID: uuid.NewString(), OriginDeviceID: claims.DeviceID.String(), EntityID: uuid.NewString(), Type: workout.CreateSetCommandType, Base: protocol.Base{Revision: &zero}, CapturedAt: start.CapturedAt, Payload: workout.CreateSetPayload{WorkoutID: start.EntityID, WorkoutExerciseID: start.Payload.Exercises[0].ID, ObservedWorkout: protocol.Base{AfterOperationID: &start.OperationID}, Load: workout.LoadInput{LoadMode: "external", WeightValue: &value, WeightUnit: &unit}, Repetitions: 10}}
	want := []workout.OfflineCommand{{Start: &start}, {CreateSet: &set}}
	raw, err := json.Marshal(map[string]any{"protocol_version": 1, "operations": want})
	if err != nil {
		t.Fatal(err)
	}
	call := func(body []byte) int {
		request := httptest.NewRequest("POST", "/api/mancuerna/v1/sync/push", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", uuid.NewString())
		response := httptest.NewRecorder()
		r.ServeHTTP(response, request)
		return response.Code
	}
	if code := call(raw); code != 200 || executor.calls != 1 || !reflect.DeepEqual(executor.commands, want) {
		t.Fatal("closed union wire input rejected or changed", code)
	}
	for _, bad := range [][]byte{
		bytes.Replace(raw, []byte(`"observed_workout":`), []byte(`"observed_Workout":`), 1),
		bytes.Replace(raw, []byte(`"weight_value":"30.0"`), []byte(`"weight_value":30.0`), 1),
		bytes.Replace(raw, []byte(`"type":"workout.set.create"`), []byte(`"type":"workout.complete"`), 1),
		bytes.Replace(raw, []byte(`"repetitions":10`), []byte(`"repetitions":null`), 1),
		bytes.Replace(raw, []byte(`"repetitions":10`), []byte(`"extra":true,"repetitions":10`), 1),
		bytes.Replace(raw, []byte(`"repetitions":10`), []byte(`"repetitions":1,"repetitions":10`), 1),
	} {
		if code := call(bad); code != 400 || executor.calls != 1 {
			t.Fatal("malformed later operation reached dispatch", code)
		}
	}
}
