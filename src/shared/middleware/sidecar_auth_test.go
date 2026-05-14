//go:build server

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/sidecartoken"
)

// fakeStore is an in-memory sidecartoken.Store used by middleware tests.
type fakeStore struct {
	cred      sidecartoken.Credential
	hash      []byte
	notFound  bool
	touched   int32
	touchErr  error
	lookupErr error
}

func (f *fakeStore) LookupActiveByHash(ctx context.Context, hash []byte) (sidecartoken.Credential, error) {
	if f.lookupErr != nil {
		return sidecartoken.Credential{}, f.lookupErr
	}
	if f.notFound {
		return sidecartoken.Credential{}, sidecartoken.ErrNotFound
	}
	return f.cred, nil
}

func (f *fakeStore) TouchLastSeen(ctx context.Context, credentialID uuid.UUID) error {
	atomic.AddInt32(&f.touched, 1)
	return f.touchErr
}

func (f *fakeStore) FindActive(context.Context, uuid.UUID, uuid.UUID) (sidecartoken.Credential, error) {
	return sidecartoken.Credential{}, sidecartoken.ErrNotFound
}
func (f *fakeStore) Insert(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, []byte, string) (sidecartoken.Credential, error) {
	return sidecartoken.Credential{}, nil
}
func (f *fakeStore) RevokeActive(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *fakeStore) RevokeIdle(context.Context, time.Time) (int64, error)     { return 0, nil }
func (f *fakeStore) ListActiveByGym(context.Context, uuid.UUID) ([]sidecartoken.Credential, error) {
	return nil, nil
}

func makeRouter(t *testing.T) (*gin.Engine, auth.TokenService, *fakeStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("test-secret")
	store := &fakeStore{}
	r := gin.New()
	g := r.Group("/x")
	g.Use(SidecarOrJWTMiddleware(tokens, store))
	g.GET("/probe", func(c *gin.Context) {
		gym, _ := GetGymID(c)
		c.JSON(http.StatusOK, gin.H{
			"gym_id": gym,
			"method": GetAuthMethod(c),
		})
	})
	return r, tokens, store
}

func TestSidecarAuth_AcceptsActiveSidecarToken(t *testing.T) {
	r, _, store := makeRouter(t)
	gymID := uuid.New()
	credID := uuid.New()
	store.cred = sidecartoken.Credential{
		ID: credID, GymID: gymID, ClientID: uuid.New(), UserID: uuid.New(),
	}
	tok, _, err := sidecartoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/x/probe", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	// Wait briefly for the fire-and-forget touch goroutine.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && atomic.LoadInt32(&store.touched) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&store.touched) != 1 {
		t.Errorf("expected TouchLastSeen called once, got %d", store.touched)
	}
}

func TestSidecarAuth_RejectsRevokedToken(t *testing.T) {
	r, _, store := makeRouter(t)
	store.notFound = true
	tok, _, _ := sidecartoken.Generate()
	req := httptest.NewRequest("GET", "/x/probe", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("status %d, want 401", rec.Code)
	}
}

func TestSidecarAuth_FallsBackToJWT(t *testing.T) {
	r, tokens, _ := makeRouter(t)
	gymID := uuid.New()
	jwt, err := tokens.GenerateAccessToken(uuid.New(), gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/x/probe", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSidecarAuth_RejectsMissingHeader(t *testing.T) {
	r, _, _ := makeRouter(t)
	req := httptest.NewRequest("GET", "/x/probe", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("status %d, want 401", rec.Code)
	}
}
