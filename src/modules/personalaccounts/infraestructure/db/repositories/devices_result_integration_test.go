//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Capture all rows that a replay could change, including the consumed proof and
// receipt. Only a digest is returned to avoid printing synthetic credential data.
func devicesResultState(t *testing.T, f *phoneChangeFixture) string {
	t.Helper()
	var state string
	err := f.owner.Raw(`SELECT md5(json_build_object(
		'account',(SELECT row_to_json(a) FROM mancuerna.accounts a WHERE id=?),
		'sessions',(SELECT json_agg(s ORDER BY id) FROM mancuerna.sessions s WHERE account_id=?),
		'devices',(SELECT json_agg(d ORDER BY id) FROM mancuerna.devices d WHERE account_id=?),
		'proofs',(SELECT json_agg(p ORDER BY id) FROM mancuerna.device_proofs p WHERE account_id=?),
		'receipts',(SELECT json_agg(r ORDER BY operation_id) FROM mancuerna.auth_receipts r WHERE account_id=?),
		'events',(SELECT json_agg(e ORDER BY id) FROM mancuerna.security_events e WHERE account_id=?)
	)::text)`, f.actor.AccountID, f.actor.AccountID, f.actor.AccountID, f.actor.AccountID, f.actor.AccountID, f.actor.AccountID).Row().Scan(&state)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestDevicesResultPostgresExactAcknowledgementCurrentAccountAndNoReplay(t *testing.T) {
	for _, scope := range []string{"others", "self", "all"} {
		t.Run(scope, func(t *testing.T) {
			f := newPhoneChangeFixture(t, 10)
			devicesTestAdd(t, f.startProof, f.actor)
			target := device.RevocationTarget{Scope: scope}
			if scope == "self" {
				target = device.RevocationTarget{DeviceID: f.actor.DeviceID}
			}
			in, _ := devicesTestInput(t, f.startProof, f.completeProof, f.actor, f.key, target, uuid.Nil, security.RevokeDevices)
			revoke := devicesTestRevoke(t, devicesTestDependencies(f.startProof))
			want, err := revoke.Execute(context.Background(), f.actor, in)
			if err != nil {
				t.Fatal(err)
			}
			read := personalResultReader(t, personalResultDependencies(t, f))
			if scope == "others" {
				got, err := read.Execute(context.Background(), f.actor, in.OperationID)
				if err != nil || got.Acknowledgement == nil || *got.Acknowledgement != want {
					t.Fatal("original live actor lost ACK", err)
				}
			} else {
				if _, err := read.Execute(context.Background(), f.actor, in.OperationID); !errors.Is(err, personalErrors.ErrSessionRevoked) {
					t.Fatal("retired actor read receipt", err)
				}
				if _, err := revoke.Execute(context.Background(), f.actor, in); !errors.Is(err, personalErrors.ErrSessionRevoked) {
					t.Fatal("retired actor replayed POST", err)
				}
			}
			// A new authorized session/device models completed reauthorization.
			// It is created after all/self; no retired session is revived.
			current := devicesTestAdd(t, f.startProof, f.actor)
			foreign := newPhoneChangeFixture(t, 10)
			before := devicesResultState(t, f)
			for i := 0; i < 3; i++ {
				got, err := read.Execute(context.Background(), current, in.OperationID)
				if err != nil || got.Session != nil || got.RecoveryKit != nil || got.Acknowledgement == nil || *got.Acknowledgement != want {
					t.Fatal("new authorized session changed ACK", err)
				}
			}
			if _, err := read.Execute(context.Background(), foreign.actor, in.OperationID); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
				t.Fatal("foreign account found result", err)
			}
			if _, err := read.Execute(context.Background(), current, uuid.New()); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
				t.Fatal("unknown operation found result", err)
			}
			if _, err := revoke.Execute(context.Background(), current, in); !errors.Is(err, personalErrors.ErrInvalidProof) {
				t.Fatal("new session replayed original POST", err)
			}
			if before != devicesResultState(t, f) {
				t.Fatal("GET or rejected replay mutated receipt, proof or effect")
			}
			if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?`, in.OperationID) != 1 || accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.security_events WHERE request_id=?`, in.OperationID) != 1 {
				t.Fatal("effect was not committed exactly once")
			}
		})
	}
}

func TestDevicesResultPostgresMalformedHistoricalReferenceIsHidden(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	foreign := newPhoneChangeFixture(t, 10)
	in, _ := devicesTestInput(t, f.startProof, f.completeProof, f.actor, f.key, device.RevocationTarget{Scope: "others"}, uuid.Nil, security.RevokeDevices)
	want, err := devicesTestRevoke(t, devicesTestDependencies(f.startProof)).Execute(context.Background(), f.actor, in)
	if err != nil {
		t.Fatal(err)
	}
	read := personalResultReader(t, personalResultDependencies(t, f))
	for _, ref := range []any{nil, uuid.New(), foreign.actor.SessionID} {
		if err := f.owner.Exec(`UPDATE mancuerna.auth_receipts SET result_reference=? WHERE operation_id=?`, ref, in.OperationID).Error; err != nil {
			t.Fatal(err)
		}
		before := devicesResultState(t, f)
		if got, err := read.Execute(context.Background(), f.actor, in.OperationID); !errors.Is(err, personalErrors.ErrReceiptNotFound) || got.Acknowledgement != nil {
			t.Fatal("invalid historical identity returned ACK", err)
		}
		if before != devicesResultState(t, f) {
			t.Fatal("read repaired malformed receipt")
		}
	}
	if err := f.owner.Exec(`UPDATE mancuerna.auth_receipts SET result_reference=?,purpose='recovery_key_confirm' WHERE operation_id=?`, f.actor.SessionID, in.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := read.Execute(context.Background(), f.actor, in.OperationID); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
		t.Fatal("another purpose returned ACK", err)
	}
	if err := f.owner.Exec(`UPDATE mancuerna.auth_receipts SET purpose='devices_revoke',result_secret_hash=? WHERE operation_id=?`, bytes.Repeat([]byte{7}, 32), in.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := read.Execute(context.Background(), f.actor, in.OperationID); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
		t.Fatal("secret-bearing receipt returned revocation ACK", err)
	}
	if err := f.owner.Exec(`UPDATE mancuerna.auth_receipts SET result_secret_hash=NULL WHERE operation_id=?`, in.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	got, err := read.Execute(context.Background(), f.actor, in.OperationID)
	if err != nil || got.Acknowledgement == nil || *got.Acknowledgement != want {
		t.Fatal("restored original receipt not readable", err)
	}
}

func TestDevicesResultHTTPPostgresLostAllResponseAndReauthorizedRead(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	devicesTestAdd(t, f.startProof, f.actor)
	in, _ := devicesTestInput(t, f.startProof, f.completeProof, f.actor, f.key, device.RevocationTarget{Scope: "all"}, uuid.Nil, security.RevokeDevices)
	auth, err := personalMiddleware.NewPersonal(f.d.Tokens, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	access, err := personalMiddleware.NewResultAccess(auth)
	if err != nil {
		t.Fatal(err)
	}
	resultHTTP, err := controllers.NewAuthResultControllerWithDeletion(personalResultReader(t, personalResultDependencies(t, f)), deletionTestReader(t, f))
	if err != nil {
		t.Fatal(err)
	}
	devicesHTTP, err := controllers.NewDevicesController(devicesTestList(t, f.startProof, time.Now), devicesTestRevoke(t, devicesTestDependencies(f.startProof)))
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	const base = "/api/mancuerna/v1"
	router.GET(base+"/auth/results/:id", access, resultHTTP.Get)
	router.POST(base+"/me/devices/revoke", auth, devicesHTTP.RevokeScope)
	call := func(method, token, secret string) (int, map[string]any) {
		t.Helper()
		path, body := base+"/auth/results/"+in.OperationID.String(), []byte(nil)
		if method == "POST" {
			path, body = base+"/me/devices/revoke", []byte(`{"scope":"all"}`)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if secret != "" {
			req.Header.Set("X-Result-Secret", secret)
		}
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", in.OperationID.String())
			req.Header.Set("X-Device-Proof", in.ProofToken)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		var result map[string]any
		if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatal("result cache or JSON contract")
		}
		return response.Code, result
	}
	code, want := call("POST", f.tokens.AccessToken, "")
	if code != 200 || len(want) != 3 || want["operation_id"] != in.OperationID.String() || want["status"] != "completed" || want["completed_at"] == "" {
		t.Fatal("POST ACK contract", code)
	}
	before := devicesResultState(t, f)
	for _, method := range []string{"GET", "POST"} {
		if code, _ := call(method, f.tokens.AccessToken, ""); code != 401 {
			t.Fatal("retired authority accepted", method, code)
		}
	}
	if before != devicesResultState(t, f) {
		t.Fatal("retired request repeated effect")
	}
	current := devicesTestAdd(t, f.startProof, f.actor)
	issue := func(actor security.AccessClaims) string {
		t.Helper()
		token, _, err := f.d.Tokens.Issue(actor.AccountID, actor.SessionID, actor.DeviceID, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	token := issue(current)
	foreign := newPhoneChangeFixture(t, 10)
	before = devicesResultState(t, f)
	for i := 0; i < 2; i++ {
		if code, got := call("GET", token, ""); code != 200 || !reflect.DeepEqual(got, want) {
			t.Fatal("reauthorized GET was not original exact ACK", code)
		}
	}
	if code, _ := call("GET", issue(foreign.actor), ""); code != 404 {
		t.Fatal("foreign HTTP receipt not hidden", code)
	}
	if code, _ := call("GET", "", ""); code != 401 {
		t.Fatal("anonymous HTTP receipt allowed", code)
	}
	secret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	if code, _ := call("GET", token, secret); code != 400 {
		t.Fatal("mixed authority accepted", code)
	}
	if code, _ := call("GET", "", secret); code != 404 {
		t.Fatal("deletion capability read revocation receipt", code)
	}
	if code, _ := call("POST", token, ""); code == 200 {
		t.Fatal("new session replayed POST")
	}
	if before != devicesResultState(t, f) {
		t.Fatal("GET or rejected HTTP replay changed durable state")
	}
}
