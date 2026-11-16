//go:build server

package controllers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"net/url"
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

type exerciseListStub struct {
	calls int
	actor security.AccessClaims
	input trainingApp.ListExercisesInput
	err   error
}

func (s *exerciseListStub) Execute(_ context.Context, a security.AccessClaims, in trainingApp.ListExercisesInput) (trainingApp.ExerciseListResult, error) {
	s.calls++
	s.actor = a
	s.input = in
	return trainingApp.ExerciseListResult{Items: []trainingApp.ExerciseDTO{}}, s.err
}

func TestExerciseHTTPClosedQueriesActorAndErrorTransport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := crypto.NewAccessTokens("exercise_test", key, map[string]ed25519.PublicKey{"exercise_test": pub})
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
	list := &exerciseListStub{}
	h, err := NewExercisesController(list)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/exercises", auth, h.List)
	call := func(path, body, header string, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest("GET", path, strings.NewReader(body))
		if authenticated {
			request.Header.Set("Authorization", "Bearer "+encoded)
		}
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
	if r := call("/exercises?query=pi%C3%A9rna&limit=1&cursor=opaque_cursor", "", "", true); r.Code != 200 || strings.TrimSpace(r.Body.String()) != "{\"items\":[],\"next_cursor\":null}" || list.actor != actor || list.input.Query != "piérna" || list.input.Cursor != "opaque_cursor" || list.input.Limit != 1 {
		t.Fatal("list transport", r.Code)
	}
	for _, query := range []string{"query=", "query=%20%20", "", "query=" + url.QueryEscape(strings.Repeat("😀", 120))} {
		if r := call("/exercises?"+query, "", "", true); r.Code != 200 {
			t.Fatal("valid Unicode/empty query", r.Code)
		}
	}
	before := list.calls
	for _, query := range []string{"status=active", "source=catalog", "recent=true", "query=a&query=b", "query=%00", "query=%FF", "limit=01", "limit=0", "limit=101", "cursor=", "unknown=1", "query=" + url.QueryEscape(strings.Repeat("😀", 121))} {
		if r := call("/exercises?"+query, "", "", true); r.Code != 400 {
			t.Fatalf("invalid query %q returned %d", query, r.Code)
		}
	}
	if r := call("/exercises", "{}", "", true); r.Code != 400 {
		t.Fatal("read body", r.Code)
	}
	for _, header := range []string{"X-Attempt-Secret", "X-Result-Secret", "X-Device-Proof", "Idempotency-Key"} {
		if r := call("/exercises", "", header, true); r.Code != 400 {
			t.Fatal("mixed capability", r.Code)
		}
	}
	if r := call("/exercises", "", "", false); r.Code != 401 {
		t.Fatal("unauthenticated global catalog", r.Code)
	}
	// The shared authentication middleware rejects malformed URL encoding
	// before the endpoint; retain that existing capability boundary.
	if r := call("/exercises?query=%", "", "", true); r.Code != 401 {
		t.Fatal("malformed URL reached endpoint", r.Code)
	}
	if _, err := exerciseListQuery("query=%"); err == nil {
		t.Fatal("endpoint parser accepted malformed URL")
	}
	if list.calls != before {
		t.Fatal("invalid request reached use case")
	}
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{protocol.ErrCursorInvalid, 400, "VALIDATION_ERROR"}, {trainingErrors.ErrInvalidExercise, 400, "VALIDATION_ERROR"},
		{protocol.ErrCursorExpired, 410, "CURSOR_EXPIRED"}, {protocol.ErrCursorEpochChanged, 410, "EPOCH_CHANGED"},
		{protocol.ErrCommandActor, 401, "SESSION_REVOKED"}, {trainingErrors.ErrTrainingPersistence, 503, "TEMPORARY_UNAVAILABLE"},
	} {
		list.err = test.err
		r := call("/exercises", "", "", true)
		var body map[string]any
		if json.Unmarshal(r.Body.Bytes(), &body) != nil || r.Code != test.status || body["code"] != test.code {
			t.Fatal("error mapping", test.code, r.Code, body["code"])
		}
	}
}
