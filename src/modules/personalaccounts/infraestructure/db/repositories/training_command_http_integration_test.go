//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	trainingApp "github.com/cuadra/cuadra-core/src/modules/training/app"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	trainingRepos "github.com/cuadra/cuadra-core/src/modules/training/infraestructure/db/repositories"
	trainingTimezone "github.com/cuadra/cuadra-core/src/modules/training/infraestructure/timezone"
	syncApp "github.com/cuadra/cuadra-core/src/shared/sync/personal/app"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	commandRepos "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/db/repositories"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestTrainingCommandHTTPPostgresClosedDispatchReplayAndPull(t *testing.T) {
	owner, actor, pull, cursors, binding := pullFixture(t)
	auth := finalizationDependencies(t)
	token, _, err := auth.Tokens.Issue(actor.AccountID, actor.SessionID, actor.DeviceID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pd := profileCommandDependencies(t)
	create, err := trainingApp.NewCreatePersonalExercise(trainingApp.CreatePersonalExerciseDependencies{UoW: pd.UoW, Exercises: trainingRepos.NewPersonalExercisePostgresRepository(), Commands: pd.Commands, Canonicalizer: pd.Canonicalizer, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	createRoutine, err := trainingApp.NewCreateRoutine(trainingApp.CreateRoutineDependencies{UoW: pd.UoW, Routines: trainingRepos.NewRoutinePostgresRepository(), Commands: pd.Commands, Stream: commandRepos.NewStream(), Canonicalizer: pd.Canonicalizer, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	saveDraft, err := trainingApp.NewSaveRoutineDraft(trainingApp.SaveRoutineDraftDependencies{UoW: pd.UoW, Routines: trainingRepos.NewRoutinePostgresRepository(), Drafts: trainingRepos.NewRoutinePostgresRepository(), Commands: pd.Commands, Stream: commandRepos.NewStream(), Canonicalizer: pd.Canonicalizer, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	publish, err := trainingApp.NewPublishRoutine(trainingApp.PublishRoutineDependencies{UoW: pd.UoW, Routines: trainingRepos.NewRoutinePostgresRepository(), Publications: trainingRepos.NewRoutinePublicationPostgresRepository(), Commands: pd.Commands, Canonicalizer: pd.Canonicalizer, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	h, err := controllers.NewCommandControllerWithDependencies(controllers.CommandDependencies{Profile: profileCommandUseCase(t, pd), Exercise: create, Routine: createRoutine, Draft: saveDraft, Publish: publish})
	if err != nil {
		t.Fatal(err)
	}
	authenticate, err := personalMiddleware.NewPersonal(auth.Tokens, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/mancuerna/v1/commands", authenticate, h.Execute)
	startWorkout, err := trainingApp.NewStartWorkout(trainingApp.StartWorkoutDependencies{UoW: pd.UoW, Versions: trainingRepos.NewRoutinePublicationPostgresRepository(), Workouts: trainingRepos.NewWorkoutPostgresRepository(), Commands: pd.Commands, Canonicalizer: pd.Canonicalizer, Timezones: trainingTimezone.NewIANA(), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	createSet, err := trainingApp.NewCreateWorkoutSet(trainingApp.CreateWorkoutSetDependencies{UoW: pd.UoW, Workouts: trainingRepos.NewWorkoutPostgresRepository(), Sets: trainingRepos.NewWorkoutSetPostgresRepository(), Commands: pd.Commands, Canonicalizer: pd.Canonicalizer, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	pushWorkouts, err := trainingApp.NewPushWorkouts(trainingApp.PushWorkoutsDependencies{UoW: pd.UoW, Commands: pd.Commands, Start: startWorkout, CreateSet: createSet, Canonicalizer: pd.Canonicalizer})
	if err != nil {
		t.Fatal(err)
	}
	pushHTTP, err := controllers.NewPushController(pushWorkouts)
	if err != nil {
		t.Fatal(err)
	}
	router.POST("/api/mancuerna/v1/sync/push", authenticate, pushHTTP.Execute)
	routineReads := trainingApp.RoutineReadDependencies{UoW: pd.UoW, Commands: pd.Commands, Routines: trainingRepos.NewRoutinePostgresRepository(), Publications: trainingRepos.NewRoutinePublicationPostgresRepository()}
	listRoutines, err := trainingApp.NewListRoutines(trainingApp.ListRoutinesDependencies{UoW: pd.UoW, Commands: pd.Commands, Stream: commandRepos.NewStream(), Routines: trainingRepos.NewRoutinePostgresRepository(), Cursors: cursors, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	getRoutine, err := trainingApp.NewGetRoutine(routineReads)
	if err != nil {
		t.Fatal(err)
	}
	getDraft, err := trainingApp.NewGetRoutineDraft(routineReads)
	if err != nil {
		t.Fatal(err)
	}
	getVersion, err := trainingApp.NewGetRoutineVersion(routineReads)
	if err != nil {
		t.Fatal(err)
	}
	readHTTP, err := controllers.NewRoutinesController(listRoutines, getRoutine, getDraft, getVersion)
	if err != nil {
		t.Fatal(err)
	}
	router.GET("/api/mancuerna/v1/routines", authenticate, readHTTP.List)
	router.GET("/api/mancuerna/v1/routines/:id", authenticate, readHTTP.Get)
	router.GET("/api/mancuerna/v1/routines/:id/draft", authenticate, readHTTP.Draft)
	router.GET("/api/mancuerna/v1/routine-versions/:id", authenticate, readHTTP.Version)
	// Exercise the GET readers through an actual loopback HTTP server, with the
	// same signed JWT and restricted PostgreSQL pools as the commands below.
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	read := func(path, bearer string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest("GET", server.URL+"/api/mancuerna/v1"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("routine read transport", err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 2097153))
		if err != nil || len(body) > 2097152 || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("invalid or cacheable routine read response", err)
		}
		return response.StatusCode, body
	}
	callAt := func(raw []byte, op, path string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/mancuerna/v1"+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", op)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cacheable command response")
		}
		var result map[string]any
		if json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatal("invalid response")
		}
		return response.Code, result
	}
	call := func(raw []byte, op, query string) (int, map[string]any) { return callAt(raw, op, "/commands"+query) }
	base := "0"
	command := exercise.CreateCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), EntityID: uuid.NewString(), Type: exercise.CreateCommandType, Base: protocol.Base{Revision: &base}, CapturedAt: "2026-10-19T00:00:00Z", Payload: exercise.CreatePayload{Name: "Sentadilla <&> e\u0301", LoadMode: exercise.External, Notes: ""}}
	raw, _ := json.Marshal(command)
	code, accepted := call(raw, command.OperationID, "")
	if code != 200 || accepted["outcome"] != "accepted" {
		t.Fatal("create failed", code, accepted["code"])
	}
	if code, replay := call(raw, command.OperationID, ""); code != 200 || !reflect.DeepEqual(accepted, replay) {
		t.Fatal("lost response not recovered exactly")
	}
	changed := bytes.Replace(raw, []byte(`"notes":""`), []byte(`"notes":"cambio"`), 1)
	if code, response := call(changed, command.OperationID, ""); code != 409 || response["code"] != "IDEMPOTENCY_MISMATCH" {
		t.Fatal("changed retry accepted", code)
	}
	for _, invalid := range [][]byte{
		bytes.Replace(raw, []byte(`"name":`), []byte(`"Name":`), 1),
		bytes.Replace(raw, []byte(`"notes":""`), []byte(`"notes":null`), 1),
		bytes.Replace(raw, []byte(`"notes":""`), []byte(`"notes":"","secret":true`), 1),
		bytes.Replace(raw, []byte(`"notes":""`), []byte(`"notes":"","notes":""`), 1),
		bytes.Replace(raw, []byte(`"type":"personal_exercise.create"`), []byte(`"type":"routine.publish"`), 1),
		bytes.Replace(raw, []byte(`"captured_at":`), []byte(`"Captured_At":`), 1),
	} {
		if code, _ := call(invalid, command.OperationID, ""); code != 400 {
			t.Fatal("open or malformed variant accepted", code)
		}
	}
	if code, _ := call(raw, uuid.NewString(), ""); code != 400 {
		t.Fatal("header/body operation mismatch accepted")
	}
	if code, _ := call(raw, command.OperationID, "?extra=1"); code != 400 {
		t.Fatal("unexpected query accepted")
	}
	oversized := []byte(`{"notes":"` + strings.Repeat("x", 2097152) + `"}`)
	if code, _ := call(oversized, uuid.NewString(), ""); code != 413 {
		t.Fatal("request size not bounded", code)
	}
	profile := profileCommand(actor, "1", "Perfil")
	profileRaw, _ := json.Marshal(profile)
	if code, response := call(profileRaw, profile.OperationID, ""); code != 200 || response["outcome"] != "accepted" {
		t.Fatal("profile dispatcher regression", code)
	}
	profile.OperationID = command.OperationID
	profileRaw, _ = json.Marshal(profile)
	if code, _ := call(profileRaw, profile.OperationID, ""); code != 409 {
		t.Fatal("operation reused across command variants")
	}
	draft := routine.CreateCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), EntityID: uuid.NewString(), Type: routine.CreateCommandType, Base: protocol.Base{Revision: &base}, CapturedAt: command.CapturedAt, Payload: routine.CreatePayload{Draft: routine.Draft{FormatVersion: 1, Title: "Rutina inicial", Days: []routine.DraftDay{{ID: uuid.NewString(), Label: "", Exercises: []routine.DraftExercise{{ID: uuid.NewString(), ExerciseRef: &routine.ExerciseRef{Source: "personal", ID: command.EntityID}}}}}}}}
	draftRaw, _ := json.Marshal(draft)
	code, draftAccepted := call(draftRaw, draft.OperationID, "")
	if code != 200 || draftAccepted["outcome"] != "accepted" {
		t.Fatal("incomplete routine rejected", code)
	}
	if code, replay := call(draftRaw, draft.OperationID, ""); code != 200 || !reflect.DeepEqual(draftAccepted, replay) {
		t.Fatal("routine replay changed")
	}
	invalidDraft := bytes.Replace(draftRaw, []byte(`"title":`), []byte(`"Title":`), 1)
	if code, _ := call(invalidDraft, draft.OperationID, ""); code != 400 {
		t.Fatal("open routine schema accepted", code)
	}
	save := routine.SaveDraftCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), EntityID: draft.EntityID, Type: routine.SaveDraftCommandType, Base: protocol.Base{AfterOperationID: &draft.OperationID}, CapturedAt: command.CapturedAt, Payload: routine.SaveDraftPayload{Content: draft.Payload.Draft}}
	save.Payload.Content.Title = "Propuesta sin publicar"
	saveRaw, _ := json.Marshal(save)
	code, saveAccepted := call(saveRaw, save.OperationID, "")
	if code != 200 || saveAccepted["outcome"] != "accepted" {
		t.Fatal("draft save dispatch failed", code, saveAccepted["code"])
	}
	if code, replay := call(saveRaw, save.OperationID, ""); code != 200 || !reflect.DeepEqual(saveAccepted, replay) {
		t.Fatal("draft lost response changed")
	}
	changedSave := bytes.Replace(saveRaw, []byte("Propuesta sin publicar"), []byte("Otra propuesta"), 1)
	if code, response := call(changedSave, save.OperationID, ""); code != 409 || response["code"] != "IDEMPOTENCY_MISMATCH" {
		t.Fatal("draft retry silently changed", code)
	}
	for _, malformed := range [][]byte{
		bytes.Replace(saveRaw, []byte(`"base_version_id":null`), []byte(`"base_version_id":""`), 1),
		bytes.Replace(saveRaw, []byte(`"content":`), []byte(`"Content":`), 1),
		bytes.Replace(saveRaw, []byte(`"base_version_id":null`), []byte(`"base_version_id":null,"published":true`), 1),
	} {
		if code, _ := call(malformed, save.OperationID, ""); code != 400 {
			t.Fatal("malformed draft save accepted", code)
		}
	}
	// Waiting for an absent local predecessor is retriable without a receipt.
	missingParent := uuid.NewString()
	pending := save
	pending.OperationID = uuid.NewString()
	pending.Base = protocol.Base{AfterOperationID: &missingParent}
	pendingRaw, _ := json.Marshal(pending)
	if code, response := call(pendingRaw, pending.OperationID, ""); code != 200 || response["outcome"] != "dependency_pending" {
		t.Fatal("missing predecessor did not remain pending", code)
	}
	stale := save
	stale.OperationID = uuid.NewString()
	staleRaw, _ := json.Marshal(stale)
	if code, response := call(staleRaw, stale.OperationID, ""); code != 200 || response["outcome"] != "conflict" {
		t.Fatal("stale draft proposal not retained", code)
	}
	cursor := issuePull(t, cursors, binding, "0", time.Now())
	page, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: cursor})
	if err != nil || len(page.Groups) != 5 || len(page.Groups[0].Changes) != 1 || len(page.Groups[2].Changes) != 2 {
		t.Fatal("journal not atomic", err)
	}
	if page.Groups[2].Changes[0].EntityType != "routine" || page.Groups[2].Changes[1].EntityType != "routine_draft" {
		t.Fatal("routine group split or incomplete")
	}
	if len(page.Groups[3].Changes) != 1 || page.Groups[3].Changes[0].EntityType != "routine_draft" || page.Groups[3].Changes[0].Revision != "2" || len(page.Groups[4].Changes) != 1 || page.Groups[4].Changes[0].EntityType != "conflict" {
		t.Fatal("save changed root or lost conflict projection")
	}
	var storedRoot struct {
		Title            string
		Revision         int64
		CurrentVersionID *string
	}
	if err = owner.Table("mancuerna.routines").Where("account_id=? AND id=?", actor.AccountID, draft.EntityID).Scan(&storedRoot).Error; err != nil || storedRoot.Title != draft.Payload.Draft.Title || storedRoot.Revision != 1 || storedRoot.CurrentVersionID != nil {
		t.Fatal("draft save published or modified root", err)
	}
	change := page.Groups[0].Changes[0]
	var projection map[string]any
	if json.Unmarshal(change.Data, &projection) != nil || change.EntityType != "personal_exercise" || change.EntityID != command.EntityID || projection["name"] != command.Payload.Name || projection["revision"] != "1" || len(projection) != 10 {
		t.Fatal("exercise projection incomplete or normalized")
	}
	var count int64
	if err = owner.Table("mancuerna.operation_receipts").Where("account_id=?", actor.AccountID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatal("invalid requests generated receipts")
	}
	complete := save
	complete.OperationID = uuid.NewString()
	complete.Base = protocol.Base{AfterOperationID: &save.OperationID}
	complete.Payload.Content.Title = "Rutina lista"
	complete.Payload.Content.Days[0].Label = "Día 1"
	sets := 3
	complete.Payload.Content.Days[0].Exercises[0].PrescribedSets = &sets
	complete.Payload.Content.Days[0].Exercises[0].PrescribedReps = "8"
	completeRaw, _ := json.Marshal(complete)
	if code, response := call(completeRaw, complete.OperationID, ""); code != 200 || response["outcome"] != "accepted" {
		t.Fatal("complete draft rejected", code, response["code"])
	}
	publication := routine.PublishCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), EntityID: draft.EntityID, Type: routine.PublishCommandType, Base: protocol.Base{AfterOperationID: &draft.OperationID}, CapturedAt: command.CapturedAt, Payload: routine.PublishPayload{DraftRevision: "3", VersionID: uuid.NewString(), Days: []routine.DayIDMap{{DraftDayID: complete.Payload.Content.Days[0].ID, ID: uuid.NewString(), Exercises: []routine.ExerciseIDMap{{DraftExerciseID: complete.Payload.Content.Days[0].Exercises[0].ID, ID: uuid.NewString()}}}}}}
	publicationRaw, _ := json.Marshal(publication)
	code, publicationAccepted := call(publicationRaw, publication.OperationID, "")
	if code != 200 || publicationAccepted["outcome"] != "accepted" {
		t.Fatal("publication dispatch failed", code, publicationAccepted["code"])
	}
	if code, replay := call(publicationRaw, publication.OperationID, ""); code != 200 || !reflect.DeepEqual(publicationAccepted, replay) {
		t.Fatal("publication lost response changed")
	}
	changedPublication := bytes.Replace(publicationRaw, []byte(publication.Payload.Days[0].ID), []byte(uuid.NewString()), 1)
	if code, response := call(changedPublication, publication.OperationID, ""); code != 409 || response["code"] != "IDEMPOTENCY_MISMATCH" {
		t.Fatal("publication identity map silently changed", code)
	}
	for _, malformed := range [][]byte{
		bytes.Replace(publicationRaw, []byte(`"draft_revision":"3"`), []byte(`"draft_revision":3`), 1),
		bytes.Replace(publicationRaw, []byte(`"draft_day_id":`), []byte(`"Draft_day_id":`), 1),
		bytes.Replace(publicationRaw, []byte(`"days":`), []byte(`"snapshot":{},"days":`), 1),
	} {
		if code, _ := call(malformed, publication.OperationID, ""); code != 400 {
			t.Fatal("malformed publication accepted", code)
		}
	}
	publishedPage, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: page.NextCursor})
	if err != nil || len(publishedPage.Groups) != 2 || len(publishedPage.Groups[0].Changes) != 1 || len(publishedPage.Groups[1].Changes) != 3 {
		t.Fatal("publication split or incomplete", err)
	}
	group := publishedPage.Groups[1]
	if group.OperationID != publication.OperationID || group.Changes[0].EntityType != "routine" || group.Changes[0].Revision != "2" || group.Changes[1].EntityType != "routine_version" || group.Changes[1].Revision != "1" || group.Changes[2].EntityType != "routine_draft" || group.Changes[2].Revision != "4" {
		t.Fatal("publication projections diverged")
	}
	version, err := protocol.DecodeRoutineVersion(group.Changes[1].Data, pd.Canonicalizer)
	if err != nil || version.ID != publication.Payload.VersionID || version.SourceDraftRevision != "3" || version.TitleSnapshot != complete.Payload.Content.Title || len(version.Days) != 1 || len(version.Days[0].Exercises) != 1 || version.Days[0].Exercises[0].NameSnapshot != command.Payload.Name {
		t.Fatal("sealed publication hash or snapshots invalid", err)
	}
	var rootProjection, draftProjection map[string]any
	if json.Unmarshal(group.Changes[0].Data, &rootProjection) != nil || json.Unmarshal(group.Changes[2].Data, &draftProjection) != nil || rootProjection["status"] != "ready" || rootProjection["current_version_id"] != version.ID || draftProjection["content"] != nil || draftProjection["deleted_at"] == nil || draftProjection["base_version_id"] != version.ID {
		t.Fatal("publication did not consume draft and advance current version")
	}
	t.Run("RoutineReadsReturnPublishedSnapshotsAndIsolateAccounts", func(t *testing.T) {
		paths := []string{"/routines/" + draft.EntityID, "/routines/" + draft.EntityID + "/draft", "/routine-versions/" + version.ID}
		for i, path := range paths {
			status, body := read(path, token)
			var got, projected map[string]any
			projectionIndex := []int{0, 2, 1}[i]
			if status != 200 || json.Unmarshal(body, &got) != nil || json.Unmarshal(group.Changes[projectionIndex].Data, &projected) != nil || !reflect.DeepEqual(got, projected) {
				t.Fatal("reader differs from complete sync projection", path, status)
			}
			if i == 2 {
				if _, err := protocol.DecodeRoutineVersion(body, pd.Canonicalizer); err != nil {
					t.Fatal("HTTP version hash", err)
				}
			}
		}
		status, body := read("/routines?query=RUT%C3%8DNA%20%20LISTA&status=active&limit=1", token)
		var listed trainingApp.RoutineListResult
		if status != 200 || json.Unmarshal(body, &listed) != nil || len(listed.Items) != 1 || listed.Items[0].ID != draft.EntityID || listed.NextCursor != nil {
			t.Fatal("published routine not discoverable", status)
		}
		_, foreign, _, _, _ := pullFixture(t)
		foreignToken, _, err := auth.Tokens.Issue(foreign.AccountID, foreign.SessionID, foreign.DeviceID, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if status, _ := read(path, foreignToken); status != 404 {
				t.Fatal("foreign routine disclosed", status)
			}
			if status, _ := read(path, ""); status != 401 {
				t.Fatal("anonymous routine disclosed", status)
			}
		}
		if status, _ := read("/routines?query=a&query=b", token); status != 400 {
			t.Fatal("duplicate filter accepted", status)
		}
	})
	if err = owner.Table("mancuerna.operation_receipts").Where("account_id=?", actor.AccountID).Count(&count).Error; err != nil || count != 7 {
		t.Fatal("publication retry or malformed input added effects", err)
	}
	newStart := func() workout.StartCommand {
		return workout.StartCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), EntityID: uuid.NewString(), Type: workout.StartCommandType, Base: protocol.Base{Revision: &base}, CapturedAt: "2026-10-18T21:00:00Z", Payload: workout.StartPayload{RoutineVersionID: version.ID, RoutineDayID: version.Days[0].ID, VersionHash: version.ContentHash, StartedAt: "2026-10-18T20:59:59.000Z", Timezone: "America/Mexico_City", Exercises: []workout.ExerciseMap{{SourceExerciseID: version.Days[0].Exercises[0].ID, ID: uuid.NewString()}}}}
	}
	batch := func(commands ...workout.StartCommand) []byte {
		value, err := json.Marshal(struct {
			ProtocolVersion int                    `json:"protocol_version"`
			Operations      []workout.StartCommand `json:"operations"`
		}{1, commands})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	start := newStart()
	malformedStart := newStart()
	malformedStart.Type = "profile.update"
	startRaw, _ := json.Marshal(start)
	if code, _ := call(startRaw, start.OperationID, ""); code != 400 {
		t.Fatal("offline producer exposed through online dispatcher")
	}
	for _, invalid := range [][]byte{
		batch(), batch(start, start), batch(start, malformedStart),
		bytes.Replace(batch(start), []byte(`"protocol_version":1`), []byte(`"protocol_version":null`), 1),
		bytes.Replace(batch(start), []byte(`"timezone":`), []byte(`"Timezone":`), 1),
		bytes.Replace(batch(start, newStart()), []byte(`"started_at":`), []byte(`"unexpected":true,"started_at":`), 1),
		batch(append(make([]workout.StartCommand, 50), start)...),
	} {
		if code, _ := callAt(invalid, uuid.NewString(), "/sync/push"); code != 400 {
			t.Fatal("invalid batch reached execution", code)
		}
	}
	if code, response := callAt(bytes.Replace(batch(start), []byte(`"protocol_version":1`), []byte(`"protocol_version":2`), 1), uuid.NewString(), "/sync/push"); code != 426 || response["code"] != "UPGRADE_REQUIRED" {
		t.Fatal("unsupported protocol accepted", code)
	}
	if code, _ := callAt(batch(start), uuid.NewString(), "/sync/push?extra=1"); code != 400 {
		t.Fatal("push query accepted")
	}
	if code, _ := callAt(oversized, uuid.NewString(), "/sync/push"); code != 413 {
		t.Fatal("push body not bounded")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.workouts WHERE account_id=?", actor.AccountID) != 0 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE account_id=?", actor.AccountID) != 7 {
		t.Fatal("prevalidation produced partial batch effects")
	}
	invalidMaterial := newStart()
	invalidMaterial.Payload.VersionHash = strings.Repeat("0", 64)
	pushRaw := batch(start, invalidMaterial)
	code, pushAccepted := callAt(pushRaw, uuid.NewString(), "/sync/push")
	if code != 200 {
		t.Fatal("push rejected typed batch", code, pushAccepted["code"])
	}
	results, ok := pushAccepted["results"].([]any)
	if !ok || len(results) != 2 || results[0].(map[string]any)["outcome"] != "accepted" || results[1].(map[string]any)["outcome"] != "rejected" {
		t.Fatal("push business result missing or reordered")
	}
	// The HTTP request ID may change; command IDs and bytes remain authoritative.
	if code, replay := callAt(pushRaw, uuid.NewString(), "/sync/push"); code != 200 || !reflect.DeepEqual(pushAccepted, replay) {
		t.Fatal("batch replay changed individual receipts")
	}
	changedStart := start
	changedStart.CapturedAt = "2026-10-18T21:00:00.000Z"
	independent := newStart()
	code, mixed := callAt(batch(changedStart, independent), uuid.NewString(), "/sync/push")
	if code != 200 {
		t.Fatal("mismatch stopped independent branch", code)
	}
	mixedResults := mixed["results"].([]any)
	firstResult := mixedResults[0].(map[string]any)
	if firstResult["outcome"] != "rejected" || firstResult["error"].(map[string]any)["code"] != "IDEMPOTENCY_MISMATCH" || mixedResults[1].(map[string]any)["outcome"] != "accepted" {
		t.Fatal("batch mismatch lost original receipt or blocked independent operation")
	}
	wrongOrigin := newStart()
	wrongOrigin.OriginDeviceID = uuid.NewString()
	if code, _ := callAt(batch(newStart(), wrongOrigin), uuid.NewString(), "/sync/push"); code != 400 {
		t.Fatal("foreign origin batch not rejected before effects", code)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE account_id=?", actor.AccountID) != 10 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.workouts WHERE account_id=?", actor.AccountID) != 2 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.workout_sets WHERE account_id=?", actor.AccountID) != 0 {
		t.Fatal("batch preflight/replay created extra effects")
	}
	workoutPage, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: publishedPage.NextCursor})
	if err != nil || len(workoutPage.Groups) != 2 {
		t.Fatal("accepted workout groups missing", err)
	}
	for _, group := range workoutPage.Groups {
		if len(group.Changes) != 1 || group.Changes[0].EntityType != "workout" || group.Changes[0].Revision != "1" {
			t.Fatal("incomplete workout snapshot")
		}
		var dto workout.DTO
		if json.Unmarshal(group.Changes[0].Data, &dto) != nil || dto.RoutineVersionID != version.ID || dto.Timezone != start.Payload.Timezone || dto.StartedAt != start.Payload.StartedAt || dto.State != "in_progress" || len(dto.Exercises) != 1 || dto.Exercises[0].NameSnapshot != command.Payload.Name || dto.Exercises[0].PrescribedSets != sets {
			t.Fatal("workout snapshot diverged from published prescription")
		}
	}
	mixedBatch := func(commands ...any) []byte {
		raw, err := json.Marshal(struct {
			ProtocolVersion int   `json:"protocol_version"`
			Operations      []any `json:"operations"`
		}{1, commands})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	parent := newStart()
	newSet := func() workout.CreateSetCommand {
		value, unit := "30.0", "kg"
		return workout.CreateSetCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), Type: workout.CreateSetCommandType, EntityID: uuid.NewString(), Base: protocol.Base{Revision: &base}, CapturedAt: "2099-10-19T10:00:00Z", Payload: workout.CreateSetPayload{WorkoutID: parent.EntityID, WorkoutExerciseID: parent.Payload.Exercises[0].ID, ObservedWorkout: protocol.Base{AfterOperationID: &parent.OperationID}, PositionHint: 0, Load: workout.LoadInput{LoadMode: "external", WeightValue: &value, WeightUnit: &unit}, Repetitions: 10}}
	}
	early, later := newSet(), newSet()
	if code, _ := callAt(bytes.Replace(mixedBatch(parent, early), []byte(`"observed_workout":`), []byte(`"observed_Workout":`), 1), uuid.NewString(), "/sync/push"); code != 400 {
		t.Fatal("malformed later set did not abort whole batch", code)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.workouts WHERE id=?", parent.EntityID) != 0 {
		t.Fatal("malformed later set left earlier effect")
	}
	setRaw, _ := json.Marshal(early)
	if code, _ := call(setRaw, early.OperationID, ""); code != 400 {
		t.Fatal("set leaked into online dispatcher")
	}
	code, firstMixed := callAt(mixedBatch(early, parent, later), uuid.NewString(), "/sync/push")
	if code != 200 {
		t.Fatal("mixed workout batch failed", code)
	}
	firstRows := firstMixed["results"].([]any)
	for i, outcome := range []string{"dependency_pending", "accepted", "accepted"} {
		if firstRows[i].(map[string]any)["outcome"] != outcome {
			t.Fatal("mixed order/dependency changed", i)
		}
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", early.OperationID) != 0 {
		t.Fatal("pending set acquired terminal receipt")
	}
	code, resumed := callAt(mixedBatch(early), uuid.NewString(), "/sync/push")
	if code != 200 || resumed["results"].([]any)[0].(map[string]any)["outcome"] != "accepted" {
		t.Fatal("pending set not accepted after parent")
	}
	code, replayed := callAt(mixedBatch(early, parent, later), uuid.NewString(), "/sync/push")
	if code != 200 {
		t.Fatal("mixed batch replay failed")
	}
	replayedRows := replayed["results"].([]any)
	if !reflect.DeepEqual(replayedRows[0], resumed["results"].([]any)[0]) || !reflect.DeepEqual(replayedRows[1:], firstRows[1:]) {
		t.Fatal("mixed replay changed committed receipts")
	}
	chained := newSet()
	chained.Payload.ObservedWorkout = protocol.Base{AfterOperationID: &early.OperationID}
	if code, response := callAt(mixedBatch(chained), uuid.NewString(), "/sync/push"); code != 200 || response["results"].([]any)[0].(map[string]any)["outcome"] != "accepted" {
		t.Fatal("set receipt did not supply workout revision")
	}
	if accessTestCount(t, owner, "SELECT revision FROM mancuerna.workouts WHERE id=?", parent.EntityID) != 4 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.workout_sets WHERE workout_id=? AND weight_value=30.000 AND weight_unit='kg'", parent.EntityID) != 3 {
		t.Fatal("independent sets lost or replay duplicated")
	}
	setPage, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: workoutPage.NextCursor})
	if err != nil || len(setPage.Groups) != 4 {
		t.Fatal("mixed effects missing from pull", err)
	}
	for i, group := range setPage.Groups[1:] {
		if len(group.Changes) != 2 || group.Changes[0].EntityType != "workout_set" || group.Changes[1].EntityType != "workout" || group.Changes[1].Revision != []string{"2", "3", "4"}[i] {
			t.Fatal("set/root group split or reordered")
		}
		var dto workout.SetDTO
		if json.Unmarshal(group.Changes[0].Data, &dto) != nil || dto.CapturedAt != "2099-10-19T10:00:00.000Z" || dto.Load.WeightValue == nil || *dto.Load.WeightValue != "30.000" || dto.WorkoutID != parent.EntityID {
			t.Fatal("set projection changed capture/load")
		}
	}
	if err = owner.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE id=?", actor.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if code, response := call(raw, command.OperationID, ""); code != 401 || response["code"] != "SESSION_REVOKED" {
		t.Fatal("revoked actor recovered receipt", code)
	}
	if code, response := callAt(pushRaw, uuid.NewString(), "/sync/push"); code != 401 || response["code"] != "SESSION_REVOKED" {
		t.Fatal("revoked batch recovered receipts", code)
	}
	t.Run("RoutineReadsRejectRevokedJWT", func(t *testing.T) {
		for _, path := range []string{"/routines", "/routines/" + draft.EntityID, "/routines/" + draft.EntityID + "/draft", "/routine-versions/" + version.ID} {
			status, body := read(path, token)
			var fault map[string]any
			if status != 401 || json.Unmarshal(body, &fault) != nil || fault["code"] != "SESSION_REVOKED" {
				t.Fatal("revoked actor read routine data", path, status)
			}
		}
	})
}
