//go:build sidecar

package controllers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	gymApp "github.com/cuadra/cuadra-core/src/modules/gyms/app"
	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	usersCtrl "github.com/cuadra/cuadra-core/src/modules/users/interfaces/controllers"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// meFixture wires the bare minimum of the AuthController against a SQLite
// UoW so we can exercise PATCH /gyms/me + POST /gyms/me/logo end-to-end.
type meFixture struct {
	t          *testing.T
	db         *sqlx.DB
	uow        sharedDomain.UnitOfWork
	router     *gin.Engine
	gymID      string
	ownerID    string
	access     string
	uploadsDir string
}

func setupMeFixture(t *testing.T) *meFixture {
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
		"../../../../../db_migrations/sqlite/004_owner_alert_configs.sql",
		"../../../../../db_migrations/sqlite/005_users_pin.sql",
		"../../../../../db_migrations/sqlite/008_gym_charge_settings.sql",
		"../../../../../db_migrations/sqlite/018_gyms_stripe_customer.sql",
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
	recorder := audit.NewSQLiteRecorder()
	tokens := auth.NewJWTService("me-test-secret")

	userRepo := usersRepoLite.NewUserSQLiteRepository()
	gymRepo := gymRepoLite.NewGymSQLiteRepository()

	signup := usersApp.NewSignupOwner(userRepo, gymRepo, uow, tokens, recorder, 30)
	owner, err := signup.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName: "Owner", Email: "owner@gym.com",
		Password: "supersecret123", PasswordConfirm: "supersecret123",
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}

	uploadsDir := filepath.Join(dir, "uploads")
	authCtrl := usersCtrl.NewAuthController(usersCtrl.AuthController{
		UpdateProfile: gymApp.NewUpdateProfile(gymRepo, uow, recorder),
		Gyms:          gymRepo,
		Users:         userRepo,
		UoW:           uow,
		Tokens:        tokens,
		UploadsDir:    uploadsDir,
	})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	authCtrl.RegisterAccountRoutes(r)
	authCtrl.RegisterUploadsRoute(r)

	return &meFixture{
		t: t, db: db, uow: uow, router: r,
		gymID: owner.GymID.String(), ownerID: owner.UserID.String(),
		access:     owner.AccessToken,
		uploadsDir: uploadsDir,
	}
}

// pngBytes builds a tiny valid PNG so http.DetectContentType returns image/png.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func multipartUpload(t *testing.T, field, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	_ = w.Close()
	return &body, w.FormDataContentType()
}

func (f *meFixture) do(method, path string, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	if body == nil {
		body = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+f.access)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// TestUploadLogo_HappyPath — POST a tiny PNG, expect 200 + the file written
// to disk + the gym row's logo_url updated to the public path. The static
// route must serve the same file.
func TestUploadLogo_HappyPath(t *testing.T) {
	f := setupMeFixture(t)
	body, ct := multipartUpload(t, "file", "logo.png", pngBytes(t))

	rec := f.do(http.MethodPost, "/api/v1/gyms/me/logo", body, ct)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Data struct {
			LogoURL string `json:"logo_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	logoURL := resp.Data.LogoURL
	if !strings.HasPrefix(logoURL, "/uploads/"+f.gymID+"/") || !strings.HasSuffix(logoURL, ".png") {
		t.Errorf("logo_url = %q, expected /uploads/<gym>/...png", logoURL)
	}

	// File present on disk under the gym subdirectory.
	rel := strings.TrimPrefix(logoURL, "/uploads/")
	onDisk := filepath.Join(f.uploadsDir, rel)
	if _, err := os.Stat(onDisk); err != nil {
		t.Errorf("expected file on disk at %s: %v", onDisk, err)
	}

	// gym.logo_url persisted.
	getRec := f.do(http.MethodGet, "/api/v1/gyms/me", nil, "")
	if getRec.Code != http.StatusOK {
		t.Fatalf("get profile = %d body=%s", getRec.Code, getRec.Body.String())
	}
	var profile struct {
		Data struct {
			LogoURL *string `json:"logo_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &profile); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	if profile.Data.LogoURL == nil || *profile.Data.LogoURL != logoURL {
		t.Errorf("profile.logo_url = %v, want %q", profile.Data.LogoURL, logoURL)
	}

	// Static route serves the asset (no auth required).
	staticReq := httptest.NewRequest(http.MethodGet, logoURL, nil)
	staticRec := httptest.NewRecorder()
	f.router.ServeHTTP(staticRec, staticReq)
	if staticRec.Code != http.StatusOK {
		t.Errorf("static serve = %d, want 200", staticRec.Code)
	}
}

// TestUploadLogo_RejectsBadMime — a text payload should be rejected by
// content sniffing even when the form filename advertises .png.
func TestUploadLogo_RejectsBadMime(t *testing.T) {
	f := setupMeFixture(t)
	body, ct := multipartUpload(t, "file", "logo.png", []byte("not actually a png"))
	rec := f.do(http.MethodPost, "/api/v1/gyms/me/logo", body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUploadLogo_RejectsOversize — bumping past 2 MiB must 400.
func TestUploadLogo_RejectsOversize(t *testing.T) {
	f := setupMeFixture(t)
	// PNG header + 2 MiB+ of pad. http.DetectContentType still says image/png
	// because of the magic bytes, but Size > maxLogoBytes trips first.
	pad := make([]byte, 2*1024*1024+128)
	header := pngBytes(t)
	body, ct := multipartUpload(t, "file", "huge.png", append(header, pad...))
	rec := f.do(http.MethodPost, "/api/v1/gyms/me/logo", body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUploadLogo_MissingFileField — missing 'file' must 400, not 500.
func TestUploadLogo_MissingFileField(t *testing.T) {
	f := setupMeFixture(t)
	body, ct := multipartUpload(t, "wrong_field", "x.png", pngBytes(t))
	rec := f.do(http.MethodPost, "/api/v1/gyms/me/logo", body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUpdateProfile_PersistsKioskSettings — PATCH /gyms/me with kiosk_volume
// + kiosk_feedback_ttl_ms must round-trip via the gym row and come back on
// the next GET.
func TestUpdateProfile_PersistsKioskSettings(t *testing.T) {
	f := setupMeFixture(t)
	patch := map[string]any{
		"kiosk_volume":          42,
		"kiosk_feedback_ttl_ms": 2750,
	}
	b, _ := json.Marshal(patch)
	rec := f.do(http.MethodPatch, "/api/v1/gyms/me", bytes.NewBuffer(b), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d body=%s", rec.Code, rec.Body.String())
	}

	getRec := f.do(http.MethodGet, "/api/v1/gyms/me", nil, "")
	if getRec.Code != http.StatusOK {
		t.Fatalf("get = %d body=%s", getRec.Code, getRec.Body.String())
	}
	var profile struct {
		Data struct {
			KioskVolume        int `json:"kiosk_volume"`
			KioskFeedbackTTLMs int `json:"kiosk_feedback_ttl_ms"`
		} `json:"data"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &profile); err != nil {
		t.Fatalf("decode: %v body=%s", err, getRec.Body.String())
	}
	if profile.Data.KioskVolume != 42 {
		t.Errorf("kiosk_volume = %d, want 42", profile.Data.KioskVolume)
	}
	if profile.Data.KioskFeedbackTTLMs != 2750 {
		t.Errorf("kiosk_feedback_ttl_ms = %d, want 2750", profile.Data.KioskFeedbackTTLMs)
	}

	// Sanity: the underlying JSON column actually stored it.
	var raw string
	if err := f.db.Get(&raw, `SELECT kiosk_settings FROM gyms WHERE id = ?`, f.gymID); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !strings.Contains(raw, `"audio_volume":42`) || !strings.Contains(raw, `"feedback_ttl_ms":2750`) {
		t.Errorf("kiosk_settings on disk = %s", raw)
	}
}
