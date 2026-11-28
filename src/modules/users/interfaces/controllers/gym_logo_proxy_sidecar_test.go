//go:build sidecar

package controllers_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	usersCtrl "github.com/cuadra/cuadra-core/src/modules/users/interfaces/controllers"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

type forwarded struct {
	method, auth, contentType string
	body                      []byte
}

func setupLogoProxy(t *testing.T, f *meFixture, cloudURL string) (*gin.Engine, *int) {
	t.Helper()
	reloads := 0
	proxy := usersCtrl.NewGymLogoSidecarProxy(usersCtrl.GymLogoSidecarProxy{
		CloudURL:    cloudURL,
		UoW:         f.uow,
		Tokens:      auth.NewJWTService("me-test-secret"),
		AgentReload: func() { reloads++ },
	})
	r := gin.New()
	proxy.RegisterRoutes(r)
	return r, &reloads
}

func pairSidecar(t *testing.T, f *meFixture) {
	t.Helper()
	err := f.uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.SetSidecarToken(context.Background(), tx, "sk_live_prueba")
	})
	if err != nil {
		t.Fatalf("set sidecar token: %v", err)
	}
}

func serveAs(r *gin.Engine, access, method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+access)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func localLogo(t *testing.T, f *meFixture) sql.NullString {
	t.Helper()
	var v sql.NullString
	if err := f.db.Get(&v, `SELECT logo_url FROM gyms WHERE id = ?`, f.gymID); err != nil {
		t.Fatalf("read local logo: %v", err)
	}
	return v
}

// El desktop no puede publicar el logo: reenvía la subida tal cual al cloud
// con el sk_live_* y refleja la URL que responde en la fila local, para que
// el siguiente ticket ya la lleve sin esperar al pull.
func TestGymLogoProxy_ReenviaAlCloudYReflejaLocal(t *testing.T) {
	f := setupMeFixture(t)
	pairSidecar(t, f)
	var got forwarded
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.auth, got.contentType = r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			_, _ = w.Write([]byte(`{"data":{"logo_url":null}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"logo_url":"https://media.example.test/logo.png"}}`))
	}))
	defer cloud.Close()
	r, reloads := setupLogoProxy(t, f, cloud.URL)

	buf, ct := multipartUpload(t, "file", "logo.png", pngBytes(t))
	sent := append([]byte(nil), buf.Bytes()...)
	rec := serveAs(r, f.access, http.MethodPost, "/api/v1/gyms/me/logo", sent, ct)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d body=%s", rec.Code, rec.Body.String())
	}
	if got.method != http.MethodPost || got.auth != "Bearer sk_live_prueba" || got.contentType != ct {
		t.Errorf("reenvío = %+v", got)
	}
	if !bytes.Equal(got.body, sent) {
		t.Error("el cuerpo multipart no llegó íntegro al cloud")
	}
	if v := localLogo(t, f); !v.Valid || v.String != "https://media.example.test/logo.png" {
		t.Errorf("logo_url local = %+v", v)
	}
	if *reloads != 1 {
		t.Errorf("AgentReload = %d, want 1", *reloads)
	}

	rec = serveAs(r, f.access, http.MethodDelete, "/api/v1/gyms/me/logo", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d body=%s", rec.Code, rec.Body.String())
	}
	if v := localLogo(t, f); v.Valid {
		t.Errorf("logo_url local tras quitar = %q, want NULL", v.String)
	}
}

// Un rechazo del cloud (p.ej. 402 del plan) se relaya tal cual y no toca la
// fila local.
func TestGymLogoProxy_ErrorDelCloudNoTocaLocal(t *testing.T) {
	f := setupMeFixture(t)
	pairSidecar(t, f)
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"status_code":402,"message":"plan"}`))
	}))
	defer cloud.Close()
	r, reloads := setupLogoProxy(t, f, cloud.URL)
	buf, ct := multipartUpload(t, "file", "logo.png", pngBytes(t))
	rec := serveAs(r, f.access, http.MethodPost, "/api/v1/gyms/me/logo", buf.Bytes(), ct)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if v := localLogo(t, f); v.Valid {
		t.Errorf("logo_url local = %q, want NULL", v.String)
	}
	if *reloads != 0 {
		t.Errorf("AgentReload = %d, want 0", *reloads)
	}
}

func TestGymLogoProxy_SinInternet503(t *testing.T) {
	f := setupMeFixture(t)
	pairSidecar(t, f)
	r, _ := setupLogoProxy(t, f, "http://127.0.0.1:1")
	buf, ct := multipartUpload(t, "file", "logo.png", pngBytes(t))
	if rec := serveAs(r, f.access, http.MethodPost, "/api/v1/gyms/me/logo", buf.Bytes(), ct); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestGymLogoProxy_SinVincular412(t *testing.T) {
	f := setupMeFixture(t)
	r, _ := setupLogoProxy(t, f, "http://127.0.0.1:1")
	if rec := serveAs(r, f.access, http.MethodDelete, "/api/v1/gyms/me/logo", nil, ""); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", rec.Code)
	}
}
