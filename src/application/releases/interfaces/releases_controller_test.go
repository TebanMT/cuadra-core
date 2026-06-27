package interfaces_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	releasesApp "github.com/cuadra/cuadra-core/src/application/releases"
	releasesIfaces "github.com/cuadra/cuadra-core/src/application/releases/interfaces"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ---------------------------------------------------------------------------
// Fakes locales — duplicados de get_latest_test.go porque están en otro
// paquete y no queremos exportar mocks como surface pública del módulo.
// ---------------------------------------------------------------------------

type fakeTx struct{}

func (fakeTx) Execute(fn func(tx sharedDomain.Transaction) error) error { return fn(fakeTx{}) }

type fakeUoW struct{}

func (fakeUoW) Begin(_ context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Commit(_ sharedDomain.Transaction) error                   { return nil }
func (fakeUoW) Rollback(_ sharedDomain.Transaction) error                 { return nil }
func (fakeUoW) Query(_ context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Command(_ context.Context, fn func(tx sharedDomain.Transaction) error) error {
	return fn(fakeTx{})
}

type fakeStore struct {
	rel       *releasesApp.Release
	inserted  []releasesApp.Release
	insertErr error // si != nil, Insert lo devuelve (ej. simular el UNIQUE)
}

func (f *fakeStore) LatestForTarget(_ sharedDomain.Transaction, _, _ string) (*releasesApp.Release, error) {
	return f.rel, nil
}

func (f *fakeStore) Insert(_ sharedDomain.Transaction, r releasesApp.Release) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = append(f.inserted, r)
	return nil
}

// ---------------------------------------------------------------------------
// Helper: arma el router con un fake store y devuelve el handler listo.
// ---------------------------------------------------------------------------

func newRouter(t *testing.T, store *fakeStore, adminToken string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	get := releasesApp.NewGetLatest(store, fakeUoW{})
	reg := releasesApp.NewRegisterRelease(store, fakeUoW{})
	ctrl := releasesIfaces.NewController(get, reg, adminToken)
	ctrl.RegisterRoutes(r)
	return r
}

// ---------------------------------------------------------------------------
// GET /releases/latest/:target/:current_version
// ---------------------------------------------------------------------------

func TestGetLatest_204_SinPublicaciones(t *testing.T) {
	r := newRouter(t, &fakeStore{rel: nil}, "")
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/releases/latest/x86_64-pc-windows-msvc/1.0.0", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestGetLatest_200_ConManifestTauri(t *testing.T) {
	rel := &releasesApp.Release{
		Version:          "1.2.4",
		TargetPlatform:   "windows",
		Channel:          "stable",
		URL:              "https://dl.entinta.app/desktop/v1.2.4/Tinta-Setup.exe",
		SignatureEd25519: "deadbeef",
		Notes:            "Fix de impresión",
		PubDate:          time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC),
		RolloutPercent:   100,
	}
	r := newRouter(t, &fakeStore{rel: rel}, "")
	// {{target}} del plugin v2 = el OS ("windows"), no el target-triple.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/releases/latest/windows/1.2.3", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["version"] != "1.2.4" {
		t.Errorf("version = %v, want 1.2.4", got["version"])
	}
	// Formato "Dynamic" (flat): url + signature al tope, SIN map `platforms`.
	// El plugin v2 buscaría platforms["windows-x86_64"] (no "windows"), así que
	// un map keyed por target no matchea — por eso servimos flat.
	if _, hasPlatforms := got["platforms"]; hasPlatforms {
		t.Errorf("el manifest NO debe incluir `platforms` (rompe el lookup del plugin v2): %+v", got)
	}
	if got["url"] != "https://dl.entinta.app/desktop/v1.2.4/Tinta-Setup.exe" {
		t.Errorf("url = %v", got["url"])
	}
	if got["signature"] != "deadbeef" {
		t.Errorf("signature = %v, want deadbeef", got["signature"])
	}
}

func TestGetLatest_204_ClienteYaEnUltima(t *testing.T) {
	rel := &releasesApp.Release{Version: "1.2.3", RolloutPercent: 100}
	r := newRouter(t, &fakeStore{rel: rel}, "")
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/releases/latest/x86_64-pc-windows-msvc/1.2.3", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestGetLatest_204_GymFueraDelStagedBucket(t *testing.T) {
	// rollout=0 garantiza que ningún gym pase el bucket, sea cual sea su hash.
	rel := &releasesApp.Release{Version: "1.2.4", RolloutPercent: 0}
	r := newRouter(t, &fakeStore{rel: rel}, "")
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/releases/latest/x86_64-pc-windows-msvc/1.2.3", nil)
	req.Header.Set("X-Tinta-Gym-ID", uuid.New().String())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestGetLatest_GymIDMalformado_NoRompe(t *testing.T) {
	// rollout=100 → siempre se ofrece, aunque ignoremos el gym_id inválido.
	rel := &releasesApp.Release{Version: "1.2.4", RolloutPercent: 100, TargetPlatform: "x86_64-pc-windows-msvc"}
	r := newRouter(t, &fakeStore{rel: rel}, "")
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/releases/latest/x86_64-pc-windows-msvc/1.2.3", nil)
	req.Header.Set("X-Tinta-Gym-ID", "not-a-uuid")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("gym_id malformado debería ignorarse silenciosamente y devolver 200 si rollout=100, got %d", w.Code)
	}
}

func TestGetLatest_204_TargetSinPublicaciones(t *testing.T) {
	// Store devuelve nil para cualquier target. Probamos uno inexistente.
	r := newRouter(t, &fakeStore{rel: nil}, "")
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/releases/latest/freebsd-riscv64/1.2.3", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestGetLatest_400_ChannelInvalido(t *testing.T) {
	r := newRouter(t, &fakeStore{rel: nil}, "")
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/releases/latest/x86_64-pc-windows-msvc/1.2.3?channel=alpha", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// ---------------------------------------------------------------------------
// POST /admin/releases
// ---------------------------------------------------------------------------

func TestAdminRegister_503_SiTokenNoConfigurado(t *testing.T) {
	r := newRouter(t, &fakeStore{}, "")
	body, _ := json.Marshal(map[string]any{
		"version": "1.2.4", "target_platform": "x86_64-pc-windows-msvc",
		"url": "https://x", "signature_ed25519": "sig",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/releases", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (token no configurado)", w.Code)
	}
}

func TestAdminRegister_401_TokenMalo(t *testing.T) {
	r := newRouter(t, &fakeStore{}, "secreto-de-pipeline")
	body, _ := json.Marshal(map[string]any{"version": "1.2.4"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/releases", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer otra-cosa")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAdminRegister_201_PersistsReleaseConToken(t *testing.T) {
	store := &fakeStore{}
	r := newRouter(t, store, "secreto-de-pipeline")
	body, _ := json.Marshal(map[string]any{
		"version":           "1.2.4",
		"target_platform":   "x86_64-pc-windows-msvc",
		"url":               "https://releases.entinta.app/win/x.exe",
		"signature_ed25519": "deadbeef",
		"notes":             "Fix",
		"rollout_percent":   5,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/releases", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secreto-de-pipeline")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
	if len(store.inserted) != 1 {
		t.Fatalf("inserted = %d, want 1", len(store.inserted))
	}
	if store.inserted[0].Version != "1.2.4" || store.inserted[0].RolloutPercent != 5 {
		t.Errorf("inserted = %+v", store.inserted[0])
	}
}

func TestAdminRegister_409_Duplicado(t *testing.T) {
	// El UNIFICADO (version, target, channel) ya existe → el store devuelve
	// ErrAlreadyExists → el handler debe responder 409 (no 500), para que el
	// re-run del pipeline sea idempotente.
	store := &fakeStore{insertErr: releasesApp.ErrAlreadyExists}
	r := newRouter(t, store, "secreto-de-pipeline")
	body, _ := json.Marshal(map[string]any{
		"version":           "1.2.4",
		"target_platform":   "windows",
		"url":               "https://dl.entinta.app/desktop/v1.2.4/Tinta-Setup.exe",
		"signature_ed25519": "deadbeef",
		"rollout_percent":   100,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/releases", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secreto-de-pipeline")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (duplicado idempotente), body=%s", w.Code, w.Body.String())
	}
}

func TestAdminRegister_400_PayloadInvalido(t *testing.T) {
	r := newRouter(t, &fakeStore{}, "secreto")
	// Sin version ni url — los dos requeridos.
	body, _ := json.Marshal(map[string]any{"target_platform": "x86_64-pc-windows-msvc"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/releases", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secreto")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}
