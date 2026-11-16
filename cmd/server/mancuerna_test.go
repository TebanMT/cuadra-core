//go:build server

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	"github.com/gin-gonic/gin"
)

func syntheticMancuernaConfig(t *testing.T) mancuernaConfig {
	t.Helper()
	randomKey := func() string {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	seed := randomKey()
	decoded, _ := personalKey(seed)
	pub := ed25519.NewKeyFromSeed(decoded).Public().(ed25519.PublicKey)
	c := mancuernaConfig{AuthDatabaseURL: "must-be-replaced-by-local-fixture", DataDatabaseURL: "must-be-replaced-by-distinct-local-fixture", MinimumAppVersion: "0.1.0", JWTKeyID: "fixture", JWTSeed: seed, JWTVerificationKeys: map[string]string{"fixture": base64.RawURLEncoding.EncodeToString(pub)}, EnvelopeKeys: mancuernaKeys{Active: "fixture", Keys: map[string]string{"fixture": randomKey()}}, RecoveryKeys: mancuernaKeys{Active: "fixture", Keys: map[string]string{"fixture": randomKey()}}, CursorKeys: mancuernaKeys{Active: "fixture", Keys: map[string]string{"fixture": randomKey()}}, PhoneLockKey: randomKey(), BudgetHashKey: randomKey(), MaxAttemptChecks: 5, ResendCooldownSeconds: 30, ReconcileAfterSeconds: 25, Limits: map[budget.Scope]mancuernaLimit{budget.Destination: {WindowSeconds: 3600, MaxSend: 3, MaxCheck: 10}, budget.Origin: {WindowSeconds: 3600, MaxSend: 3, MaxCheck: 10}, budget.Service: {WindowSeconds: 3600, MaxSend: 3, MaxCheck: 10}}}
	c.RecoveryLimit = mancuernaRecoveryLimit{WindowSeconds: 60, MaxAttempts: 6}
	c.Verify.AccountSID = "AC" + strings.Repeat("0", 32)
	c.Verify.ServiceSID = "VA" + strings.Repeat("0", 32)
	c.Verify.Username = c.Verify.AccountSID
	c.Verify.Password = "synthetic-provider-password"
	c.Verify.ValiditySeconds = 600
	c.Verify.TimeoutSeconds = 20
	return c
}
func TestMancuernaConfigurationIsExplicitPrivateAndNeverLogsSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("MANCUERNA_CONFIG_FILE", "")
	// Existing administrative configuration cannot turn personal routes on.
	t.Setenv("DATABASE_URL", "forbidden-administrative-database")
	t.Setenv("TWILIO_AUTH_TOKEN", "must-never-be-used")
	router := gin.New()
	close, err := configureMancuerna(router)
	if err != nil {
		t.Fatal("absent personal config must be disabled")
	}
	close()
	if len(router.Routes()) != 0 {
		t.Fatal("personal route implicitly enabled")
	}
	c := syntheticMancuernaConfig(t)
	for _, formatted := range []string{fmt.Sprint(c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(formatted, c.JWTSeed) || strings.Contains(formatted, c.Verify.Password) {
			t.Fatal("config format disclosed secret")
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "private.json")
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readMancuernaConfig(path); err != nil {
		t.Fatal("private valid file rejected")
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = readMancuernaConfig(path); err == nil {
		t.Fatal("publicly readable secret configuration accepted")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{append(append([]byte{}, raw...), []byte(` {}`)...), []byte(`{"unknown":"secret"}`), bytes.Repeat([]byte("x"), 65537)} {
		if err = os.WriteFile(path, invalid, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = readMancuernaConfig(path); err == nil {
			t.Fatal("invalid private configuration accepted")
		}
	}
}
func TestMancuernaPanicAndQueryNeverReachDefaultDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var logs bytes.Buffer
	oldOut, oldErr := gin.DefaultWriter, gin.DefaultErrorWriter
	gin.DefaultWriter = &logs
	gin.DefaultErrorWriter = &logs
	defer func() { gin.DefaultWriter = oldOut; gin.DefaultErrorWriter = oldErr }()
	router := newServerRouter()
	secret := "synthetic-attempt-secret-do-not-log"
	router.GET("/api/mancuerna/v1/panic", func(*gin.Context) { panic(secret) })
	router.GET("/ordinary", func(c *gin.Context) { c.Status(200) })
	req := httptest.NewRequest("GET", "/api/mancuerna/v1/panic?access_token="+secret, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Attempt-Secret", secret)
	req.Header.Set("X-Result-Secret", secret)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("unsafe panic response")
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(w.Body.String(), secret) {
		t.Fatal("sensitive diagnostics escaped")
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/ordinary", nil))
	if !strings.Contains(logs.String(), "/ordinary") {
		t.Fatal("existing diagnostics removed")
	}
}
