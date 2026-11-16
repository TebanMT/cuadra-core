//go:build server && integration

package repositories

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	syncApp "github.com/cuadra/cuadra-core/src/shared/sync/personal/app"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	streamRepos "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/db/repositories"
	cursorCrypto "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestSnapshotHTTPPostgresManifestPagesPullAndRevokedJWT(t *testing.T) {
	d := finalizationDependencies(t)
	_, in, _ := verifiedAccessFixture(t, d, accessTestPhone(), nil)
	finalize, err := personalApp.NewFinalizeAccess(d)
	if err != nil {
		t.Fatal(err)
	}
	signup, err := finalize.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := d.Tokens.Verify(signup.Authorized.Session.AccessToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pd := profileCommandDependencies(t)
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cursors, err := cursorCrypto.NewCursorSigner("snapshot-http", map[string][]byte{"snapshot-http": key})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := streamRepos.NewSnapshots(crypto.JSONCanonicalizer{})
	if err != nil {
		t.Fatal(err)
	}
	caps := protocol.Capabilities{ProtocolVersion: 1, MinimumProtocolVersion: 1, MinimumAppVersion: "0.1.0", MaxPushOperations: 50, MaxRequestBytes: 2097152, MaxGroupBytes: 1048576, Features: []string{}}
	create, err := syncApp.NewCreateSnapshot(syncApp.CreateSnapshotDependencies{UoW: pd.UoW.(shared.PersonalSnapshotUnitOfWork), Snapshots: snapshots, Cursors: cursors, Capabilities: caps, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	page, err := syncApp.NewGetSnapshotPage(syncApp.GetSnapshotPageDependencies{UoW: pd.UoW, Snapshots: snapshots, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	pull, err := syncApp.NewPull(syncApp.PullDependencies{UoW: pd.UoW, Commands: pd.Commands, Stream: streamRepos.NewStream(), Cursors: cursors, Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	h, err := controllers.NewSyncSnapshotController(create, page)
	if err != nil {
		t.Fatal(err)
	}
	ph, err := controllers.NewSyncPullController(pull)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := personalMiddleware.NewPersonal(d.Tokens, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/mancuerna/v1/sync/snapshots", middleware, h.Create)
	router.GET("/api/mancuerna/v1/sync/snapshots/:id/pages/:number", middleware, h.Page)
	router.GET("/api/mancuerna/v1/sync/pull", middleware, ph.Get)
	id := uuid.NewString()
	call := func(method, path, raw, op string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, "/api/mancuerna/v1"+path, strings.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+signup.Authorized.Session.AccessToken)
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", op)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Request-ID") == "" {
			t.Fatal("missing private-response headers")
		}
		var body map[string]any
		if json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatal("invalid public JSON")
		}
		return w.Code, body
	}
	for _, bad := range []string{`{}`, `{"protocol_version":2}`, `{"protocol_version":1,"Protocol_version":1}`, `{"protocol_version":1,"protocol_version":1}`, `{"protocol_version":1,"other":true}`} {
		if code, _ := call("POST", "/sync/snapshots", bad, uuid.NewString()); code != 400 {
			t.Fatal("invalid body accepted", code)
		}
	}
	code, manifest := call("POST", "/sync/snapshots", `{"protocol_version":1}`, id)
	if code != 200 || len(manifest) != 8 || manifest["id"] != id || manifest["through_sequence"] != "0" || manifest["page_count"] != float64(1) {
		t.Fatal("manifest", code, manifest)
	}
	path := "/sync/snapshots/" + id + "/pages/0"
	code, first := call("GET", path, "", "")
	if code != 200 || len(first) != 4 || first["snapshot_id"] != id || len(first["records"].([]any)) != 1 {
		t.Fatal("page", code, first)
	}
	raw, err := json.Marshal(first["records"])
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := (crypto.JSONCanonicalizer{}).Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(canonical)
	if first["hash"] != hex.EncodeToString(hash[:]) || manifest["page_hashes"].([]any)[0] != first["hash"] {
		t.Fatal("hash is not canonical records hash")
	}
	update := profileCommandUseCase(t, pd)
	if result, err := update.Execute(context.Background(), actor, profileCommand(actor, "1", "Después de la copia")); err != nil || result.Outcome != "accepted" {
		t.Fatal("profile command", err)
	}
	code, retry := call("POST", "/sync/snapshots", `{"protocol_version":1}`, id)
	if code != 200 || !reflect.DeepEqual(manifest, retry) {
		t.Fatal("retry changed original manifest", code)
	}
	code, retry = call("GET", path, "", "")
	if code != 200 || !reflect.DeepEqual(first, retry) {
		t.Fatal("page mutated", code)
	}
	code, changes := call("GET", "/sync/pull?cursor="+manifest["cursor"].(string), "", "")
	if code != 200 || len(changes["groups"].([]any)) != 1 || changes["groups"].([]any)[0].(map[string]any)["sequence"] != "1" {
		t.Fatal("bootstrap cursor lost later commit", code, changes)
	}
	for _, suffix := range []string{"01", "-1", "100000", "0?extra=1"} {
		if code, _ := call("GET", "/sync/snapshots/"+id+"/pages/"+suffix, "", ""); code != 400 {
			t.Fatal("invalid page accepted", code)
		}
	}
	if code, _ := call("GET", "/sync/snapshots/"+id+"/pages/1", "", ""); code != 404 {
		t.Fatal("missing page", code)
	}
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	if err = owner.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE id=?", actor.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if code, body := call("GET", path, "", ""); code != 401 || body["code"] != "SESSION_REVOKED" {
		t.Fatal("revoked snapshot read", code)
	}
	if code, body := call("POST", "/sync/snapshots", `{"protocol_version":1}`, id); code != 401 || body["code"] != "SESSION_REVOKED" {
		t.Fatal("revoked manifest replay", code)
	}
}
