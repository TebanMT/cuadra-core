//go:build sidecar

package controllers_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	usersCtrl "github.com/cuadra/cuadra-core/src/modules/users/interfaces/controllers"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// setupSidecarTestDB mirrors the agent_test fixture (SQLite + migrations).
func setupSidecarTestDB(t *testing.T) (*sqlx.DB, sharedDomain.UnitOfWork) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sqlx.Open("sqlite3", dbPath+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	for _, m := range []string{
		"../../../../../db_migrations/sqlite/001_init_schema.sql",
		"../../../../../db_migrations/sqlite/002_notifications.sql",
		"../../../../../db_migrations/sqlite/003_sync_local.sql",
	} {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}
	uow := sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue())
	return db, uow
}

// fakeCloud captures the proxied request and returns a configurable response.
type fakeCloud struct {
	srv          *httptest.Server
	gotPath      string
	gotMethod    string
	gotHeaders   http.Header
	gotBody      []byte
	respStatus   int
	respBody     []byte
	hits         atomic.Int32
}

func newFakeCloud(t *testing.T) *fakeCloud {
	t.Helper()
	c := &fakeCloud{respStatus: 200, respBody: []byte(`{"data":{}}`)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		c.gotPath = r.URL.Path
		c.gotMethod = r.Method
		c.gotHeaders = r.Header
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		c.gotBody = buf.Bytes()
		w.WriteHeader(c.respStatus)
		_, _ = w.Write(c.respBody)
	})
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

func newProxyRouter(t *testing.T, cfg usersCtrl.SidecarAuthProxy) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	p := usersCtrl.NewSidecarAuthProxy(cfg)
	p.RegisterRoutes(r)
	return r
}

func doProxyReq(r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	var buf *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestProxy_LoginForwardsAndAbsorbsToken — happy path. Cloud returns a
// sidecar_token; the proxy persists it to sync_state and triggers the
// agent reload callback.
func TestProxy_LoginForwardsAndAbsorbsToken(t *testing.T) {
	_, uow := setupSidecarTestDB(t)
	cloud := newFakeCloud(t)
	cloud.respStatus = 200
	cloud.respBody = []byte(`{"status_code":200,"data":{"user_id":"` + uuid.NewString() + `","gym_id":"` + uuid.NewString() + `","role":"owner","access_token":"jwt","refresh_token":"jwt-refresh","setup_completed":true,"subscription_plan":"trial","gyms":[{"id":"` + uuid.NewString() + `","name":"Test Gym"}],"sidecar_token":"sk_live_abc"}}`)

	reloaded := atomic.Int32{}
	r := newProxyRouter(t, usersCtrl.SidecarAuthProxy{
		CloudURL:    cloud.srv.URL,
		UoW:         uow,
		LocalTokens: auth.NewJWTService("test-secret"),
		ClientID:    uuid.New(),
		DeviceLabel: "laptop-1",
		AgentReload: func() { reloaded.Add(1) },
	})

	rec := doProxyReq(r, "POST", "/api/v1/auth/login", map[string]any{
		"email": "owner@gym.com", "password": "secret",
	})
	if rec.Code != 200 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}

	// Bootstrap headers were forwarded.
	if cloud.gotHeaders.Get("X-Cuadra-Client") != "sidecar" {
		t.Errorf("missing X-Cuadra-Client header")
	}
	if cloud.gotHeaders.Get("X-Cuadra-Client-ID") == "" {
		t.Errorf("missing X-Cuadra-Client-ID header")
	}
	if cloud.gotHeaders.Get("X-Cuadra-Device-Label") != "laptop-1" {
		t.Errorf("missing X-Cuadra-Device-Label header")
	}

	// sidecar_token persisted to sync_state.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && reloaded.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if reloaded.Load() != 1 {
		t.Errorf("expected AgentReload to be called once, got %d", reloaded.Load())
	}
}

// TestProxy_LoginOfflineUsesCachedCredentials — cloud unreachable; the
// proxy falls back to the bcrypt cache populated by a previous successful
// login.
func TestProxy_LoginOfflineUsesCachedCredentials(t *testing.T) {
	_, uow := setupSidecarTestDB(t)
	tokens := auth.NewJWTService("offline-test-secret")

	// First, prime the cache via an online login.
	cloud := newFakeCloud(t)
	gymID := uuid.NewString()
	userID := uuid.NewString()
	cloud.respStatus = 200
	cloud.respBody = []byte(fmt.Sprintf(`{"status_code":200,"data":{"user_id":%q,"gym_id":%q,"role":"owner","access_token":"jwt","refresh_token":"r","setup_completed":true,"subscription_plan":"trial","gyms":[{"id":%q,"name":"Gym"}]}}`, userID, gymID, gymID))
	r := newProxyRouter(t, usersCtrl.SidecarAuthProxy{
		CloudURL:    cloud.srv.URL,
		UoW:         uow,
		LocalTokens: tokens,
		ClientID:    uuid.New(),
	})
	if rec := doProxyReq(r, "POST", "/api/v1/auth/login", map[string]any{
		"email": "owner@gym.com", "password": "secret",
	}); rec.Code != 200 {
		t.Fatalf("priming login status %d body=%s", rec.Code, rec.Body.String())
	}

	// Now build a fresh proxy pointing at a dead cloud URL to simulate
	// offline. The cache from above survives in sync_state.
	rOffline := newProxyRouter(t, usersCtrl.SidecarAuthProxy{
		CloudURL:    "http://127.0.0.1:1", // refused
		HTTPClient:  &http.Client{Timeout: 100 * time.Millisecond},
		UoW:         uow,
		LocalTokens: tokens,
		ClientID:    uuid.New(),
	})

	// Wrong password → 401.
	if rec := doProxyReq(rOffline, "POST", "/api/v1/auth/login", map[string]any{
		"email": "owner@gym.com", "password": "wrong",
	}); rec.Code != 401 {
		t.Errorf("wrong password offline = %d, want 401", rec.Code)
	}
	// Right password → 200 with a freshly minted local JWT.
	rec := doProxyReq(rOffline, "POST", "/api/v1/auth/login", map[string]any{
		"email": "owner@gym.com", "password": "secret",
	})
	if rec.Code != 200 {
		t.Fatalf("offline login = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "access_token") {
		t.Errorf("response missing access_token: %s", rec.Body.String())
	}
}

// TestProxy_SignupOfflineFails503 — signup MUST require internet
// (ADR-008 §3.4 — no cache priming possible).
func TestProxy_SignupOfflineFails503(t *testing.T) {
	_, uow := setupSidecarTestDB(t)
	r := newProxyRouter(t, usersCtrl.SidecarAuthProxy{
		CloudURL:    "http://127.0.0.1:1",
		HTTPClient:  &http.Client{Timeout: 50 * time.Millisecond},
		UoW:         uow,
		LocalTokens: auth.NewJWTService("test"),
		ClientID:    uuid.New(),
	})
	rec := doProxyReq(r, "POST", "/api/v1/auth/signup", map[string]any{
		"full_name": "X", "email": "x@x.com", "password": "abc12345", "password_confirm": "abc12345",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("offline signup status = %d, want 503", rec.Code)
	}
}

// TestProxy_LogoutSucceedsOffline — logout clears local state even when
// cloud is unreachable; the sync agent keeps its sidecar_token (ADR-008
// §3.4 logout column).
func TestProxy_LogoutSucceedsOffline(t *testing.T) {
	_, uow := setupSidecarTestDB(t)
	r := newProxyRouter(t, usersCtrl.SidecarAuthProxy{
		CloudURL:    "http://127.0.0.1:1",
		HTTPClient:  &http.Client{Timeout: 50 * time.Millisecond},
		UoW:         uow,
		LocalTokens: auth.NewJWTService("test"),
		ClientID:    uuid.New(),
	})
	if rec := doProxyReq(r, "POST", "/api/v1/auth/logout", nil); rec.Code != http.StatusNoContent {
		t.Errorf("offline logout = %d, want 204", rec.Code)
	}
}

// TestProxy_SignupMirrorsCloudIdentityIntoLocalDB — regression: after
// the proxy forwards a signup to cloud, the gym + user rows must exist
// in local SQLite immediately so the wizard's first PATCH /gyms/me/setup
// succeeds without waiting for the next sync pull.
func TestProxy_SignupMirrorsCloudIdentityIntoLocalDB(t *testing.T) {
	db, uow := setupSidecarTestDB(t)
	cloud := newFakeCloud(t)
	gymID := uuid.NewString()
	userID := uuid.NewString()
	cloud.respStatus = 201
	cloud.respBody = []byte(fmt.Sprintf(
		`{"status_code":201,"data":{"user_id":%q,"gym_id":%q,"role":"owner","access_token":"jwt","refresh_token":"r","setup_completed":false,"gyms":[{"id":%q,"name":"Brand New Gym"}],"sidecar_token":"sk_live_xyz"}}`,
		userID, gymID, gymID,
	))
	r := newProxyRouter(t, usersCtrl.SidecarAuthProxy{
		CloudURL:    cloud.srv.URL,
		UoW:         uow,
		LocalTokens: auth.NewJWTService("test"),
		ClientID:    uuid.New(),
	})
	rec := doProxyReq(r, "POST", "/api/v1/auth/signup", map[string]any{
		"full_name": "Owner Name", "email": "owner@gym.com",
		"password": "supersecret", "password_confirm": "supersecret",
	})
	if rec.Code != 201 {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}

	var gymRow struct {
		ID   string
		Name *string
	}
	if err := db.Get(&gymRow, `SELECT id, name FROM gyms WHERE id = ?`, gymID); err != nil {
		t.Fatalf("gym not mirrored locally: %v", err)
	}
	if gymRow.Name == nil || *gymRow.Name != "Brand New Gym" {
		t.Errorf("gym name not propagated: %v", gymRow.Name)
	}

	var userRow struct {
		ID    string
		Email string
		Role  string
	}
	if err := db.Get(&userRow, `SELECT id, email, role FROM users WHERE id = ?`, userID); err != nil {
		t.Fatalf("user not mirrored locally: %v", err)
	}
	if userRow.Email != "owner@gym.com" || userRow.Role != "owner" {
		t.Errorf("user fields wrong: %+v", userRow)
	}

	// Sync queue must NOT contain the mirrored rows — they came from cloud
	// and re-pushing would echo.
	var queueCount int
	if err := db.Get(&queueCount, `SELECT COUNT(*) FROM sync_queue WHERE entity_id IN (?, ?)`, gymID, userID); err != nil {
		t.Fatalf("count queue: %v", err)
	}
	if queueCount != 0 {
		t.Errorf("expected 0 sync_queue rows for mirrored identity, got %d", queueCount)
	}
}

// TestProxy_RefreshAlwaysSucceedsWithCachedLogin — desktop is a kiosko;
// /auth/refresh must NEVER kick the operator out as long as cached_login
// exists. The endpoint mints a fresh JWT pair locally, no cloud round-trip.
func TestProxy_RefreshAlwaysSucceedsWithCachedLogin(t *testing.T) {
	_, uow := setupSidecarTestDB(t)
	tokens := auth.NewJWTService("refresh-test-secret")
	cloud := newFakeCloud(t)
	gymID := uuid.NewString()
	userID := uuid.NewString()
	cloud.respStatus = 200
	cloud.respBody = []byte(fmt.Sprintf(
		`{"status_code":200,"data":{"user_id":%q,"gym_id":%q,"role":"owner","access_token":"jwt","refresh_token":"r","setup_completed":true,"subscription_plan":"trial","gyms":[{"id":%q,"name":"Gym"}]}}`,
		userID, gymID, gymID,
	))
	r := newProxyRouter(t, usersCtrl.SidecarAuthProxy{
		CloudURL:    cloud.srv.URL,
		UoW:         uow,
		LocalTokens: tokens,
		ClientID:    uuid.New(),
	})
	// Prime cached_login.
	if rec := doProxyReq(r, "POST", "/api/v1/auth/login", map[string]any{
		"email": "owner@gym.com", "password": "secret",
	}); rec.Code != 200 {
		t.Fatalf("priming login = %d body=%s", rec.Code, rec.Body.String())
	}

	// Even when cloud is dead, refresh must succeed from cache.
	rOffline := newProxyRouter(t, usersCtrl.SidecarAuthProxy{
		CloudURL:    "http://127.0.0.1:1",
		HTTPClient:  &http.Client{Timeout: 50 * time.Millisecond},
		UoW:         uow,
		LocalTokens: tokens,
		ClientID:    uuid.New(),
	})
	rec := doProxyReq(rOffline, "POST", "/api/v1/auth/refresh", map[string]any{
		"refresh_token": "anything-the-desktop-stored",
	})
	if rec.Code != 200 {
		t.Fatalf("refresh status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "access_token") || !strings.Contains(rec.Body.String(), "refresh_token") {
		t.Errorf("refresh response missing tokens: %s", rec.Body.String())
	}
}

// TestProxy_RefreshFailsWhenNoCachedLogin — fresh laptop with no prior
// online login → must 401 so the desktop redirects to /auth/login. This
// is the only legitimate "kick out" path.
func TestProxy_RefreshFailsWhenNoCachedLogin(t *testing.T) {
	_, uow := setupSidecarTestDB(t)
	r := newProxyRouter(t, usersCtrl.SidecarAuthProxy{
		CloudURL:    "http://127.0.0.1:1",
		HTTPClient:  &http.Client{Timeout: 50 * time.Millisecond},
		UoW:         uow,
		LocalTokens: auth.NewJWTService("test"),
		ClientID:    uuid.New(),
	})
	rec := doProxyReq(r, "POST", "/api/v1/auth/refresh", map[string]any{
		"refresh_token": "irrelevant",
	})
	if rec.Code != 401 {
		t.Errorf("refresh without cache = %d, want 401", rec.Code)
	}
}

// TestProxy_LoginRelaysCloud401 — cloud answered with 401, proxy must
// pass it through verbatim instead of falling back to cache.
func TestProxy_LoginRelaysCloud401(t *testing.T) {
	_, uow := setupSidecarTestDB(t)
	cloud := newFakeCloud(t)
	cloud.respStatus = 401
	cloud.respBody = []byte(`{"status_code":401,"exception":{"code":"INVALID_CREDENTIALS"}}`)
	r := newProxyRouter(t, usersCtrl.SidecarAuthProxy{
		CloudURL:    cloud.srv.URL,
		UoW:         uow,
		LocalTokens: auth.NewJWTService("test"),
		ClientID:    uuid.New(),
	})
	rec := doProxyReq(r, "POST", "/api/v1/auth/login", map[string]any{
		"email": "x@x.com", "password": "wrong",
	})
	if rec.Code != 401 {
		t.Errorf("relayed status = %d, want 401", rec.Code)
	}
}
