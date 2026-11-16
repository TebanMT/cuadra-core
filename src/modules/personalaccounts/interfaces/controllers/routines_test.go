//go:build server

package controllers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	trainingApp "github.com/cuadra/cuadra-core/src/modules/training/app"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type routineListStub struct {
	calls int
	actor security.AccessClaims
	input trainingApp.ListRoutinesInput
	err   error
}

func (s *routineListStub) Execute(_ context.Context, a security.AccessClaims, in trainingApp.ListRoutinesInput) (trainingApp.RoutineListResult, error) {
	s.calls++
	s.actor = a
	s.input = in
	return trainingApp.RoutineListResult{Items: []trainingApp.RoutineDTO{}}, s.err
}

type routineGetStub struct {
	calls int
	actor security.AccessClaims
	id    uuid.UUID
	err   error
}

func (s *routineGetStub) Execute(_ context.Context, a security.AccessClaims, id uuid.UUID) (trainingApp.RoutineDTO, error) {
	s.calls++
	s.actor = a
	s.id = id
	return trainingApp.RoutineDTO{ID: id.String(), Revision: "9007199254740993"}, s.err
}

type routineDraftStub struct {
	calls int
	err   error
}

func (s *routineDraftStub) Execute(_ context.Context, _ security.AccessClaims, id uuid.UUID) (trainingApp.RoutineDraftDTO, error) {
	s.calls++
	at := "2026-10-23T12:00:00.000Z"
	return trainingApp.RoutineDraftDTO{ID: id.String(), Revision: "2", DeletedAt: &at}, s.err
}

type routineVersionStub struct {
	calls int
	err   error
}

func (s *routineVersionStub) Execute(_ context.Context, _ security.AccessClaims, id uuid.UUID) (protocol.RoutineVersionDTO, error) {
	s.calls++
	return protocol.RoutineVersionDTO{ID: id.String(), Revision: "1"}, s.err
}

func TestRoutineHTTPClosedQueriesAndActorTransport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := crypto.NewAccessTokens("routine_test", key, map[string]ed25519.PublicKey{"routine_test": pub})
	if err != nil {
		t.Fatal(err)
	}
	encoded, actor, err := tokens.Issue(uuid.New(), uuid.New(), uuid.New(), now)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := personalMiddleware.NewPersonal(tokens, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	list, get, draft, version := &routineListStub{}, &routineGetStub{}, &routineDraftStub{}, &routineVersionStub{}
	h, err := NewRoutinesController(list, get, draft, version)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/routines", auth, h.List)
	router.GET("/routines/:id", auth, h.Get)
	router.GET("/routines/:id/draft", auth, h.Draft)
	router.GET("/routine-versions/:id", auth, h.Version)
	call := func(path, body, header string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest("GET", path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+encoded)
		if header != "" {
			request.Header.Set(header, "forbidden")
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Request-ID") == "" {
			t.Fatal("missing private-response headers")
		}
		return response
	}
	if r := call("/routines?status=all&query=pi%C3%A9rna&limit=1", "", ""); r.Code != 200 || strings.TrimSpace(r.Body.String()) != "{\"items\":[],\"next_cursor\":null}" || list.actor != actor || list.input.Query != "piérna" || list.input.Status != "all" || list.input.Limit != 1 {
		t.Fatal("list transport", r.Code)
	}
	for _, query := range []string{"query=", "query=%20%20", ""} {
		if r := call("/routines?"+query, "", ""); r.Code != 200 {
			t.Fatal("valid empty search rejected", r.Code)
		}
	}
	count := list.calls
	for _, query := range []string{"status=ready", "status=", "status=active&status=all", "query=a&query=b", "query=%00", "query=%FF", "limit=01", "limit=0", "limit=101", "cursor=", "unknown=1", "query=" + strings.Repeat("a", 121)} {
		if r := call("/routines?"+query, "", ""); r.Code != 400 {
			t.Fatalf("invalid query %q status %d", query, r.Code)
		}
	}
	if list.calls != count {
		t.Fatal("invalid query reached use case")
	}
	id := uuid.New()
	for _, path := range []string{"/routines/" + id.String(), "/routines/" + id.String() + "/draft", "/routine-versions/" + id.String()} {
		if r := call(path, "", ""); r.Code != 200 {
			t.Fatal("single read", r.Code)
		}
		for _, suffix := range []string{"?unknown=1", "?query=x"} {
			if r := call(path+suffix, "", ""); r.Code != 400 {
				t.Fatal("single read accepted query", r.Code)
			}
		}
	}
	if get.actor != actor || get.id != id {
		t.Fatal("actor or path not forwarded")
	}
	if r := call("/routines/"+strings.ToUpper(id.String()), "", ""); r.Code != 400 {
		t.Fatal("noncanonical ID accepted", r.Code)
	}
	for _, path := range []string{"/routines", "/routines/" + id.String(), "/routines/" + id.String() + "/draft", "/routine-versions/" + id.String()} {
		if r := call(path, "{}", ""); r.Code != 400 {
			t.Fatal("read body accepted", r.Code)
		}
		for _, header := range []string{"X-Attempt-Secret", "X-Result-Secret", "X-Device-Proof", "Idempotency-Key"} {
			if r := call(path, "", header); r.Code != 400 {
				t.Fatal("mixed capability", header, r.Code)
			}
		}
	}
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{{protocol.ErrCursorInvalid, 400, "VALIDATION_ERROR"}, {protocol.ErrCursorExpired, 410, "CURSOR_EXPIRED"}, {protocol.ErrCursorEpochChanged, 410, "EPOCH_CHANGED"}, {protocol.ErrCommandActor, 401, "SESSION_REVOKED"}, {trainingErrors.ErrRoutineNotFound, 404, "NOT_FOUND"}, {trainingErrors.ErrTrainingPersistence, 503, "TEMPORARY_UNAVAILABLE"}} {
		list.err = test.err
		r := call("/routines", "", "")
		var body map[string]any
		if json.Unmarshal(r.Body.Bytes(), &body) != nil || r.Code != test.status || body["code"] != test.code {
			t.Fatal("error mapping", test.code, r.Code, body["code"])
		}
	}
}
