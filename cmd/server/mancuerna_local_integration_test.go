//go:build server && integration

package main

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"

	verify "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/verification"
	"github.com/gin-gonic/gin"
)

func TestMancuernaLocalPreparesOnlyLoopbackWithExistingGuardAndRestrictedPools(t *testing.T) {
	// Read only the explicit fixture DSNs. No administrative DSN fallback.
	auth, data := os.Getenv("MANCUERNA_TEST_AUTH_DSN"), os.Getenv("MANCUERNA_TEST_DATA_DSN")
	if auth == "" || data == "" {
		t.Skip("explicit local PostgreSQL fixture required")
	}
	clearLocalPGEnvironment(t)
	if _, err := localPersonalDSN(auth); err != nil {
		t.Fatal("invalid local auth fixture")
	}
	if _, err := localPersonalDSN(data); err != nil {
		t.Fatal("invalid local data fixture")
	}
	c := syntheticMancuernaConfig(t)
	c.AuthDatabaseURL, c.DataDatabaseURL = auth, data
	guardConfig := syntheticGuardConfig(t, &c)
	if err := verify.InitializeLocalVerifyLedger(guardConfig); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANCUERNA_CONFIG_FILE", writeLocalConfig(t, c))
	t.Setenv("DATABASE_URL", "forbidden-administrative-database")
	t.Setenv("PORT", "0.0.0.0:9999")
	server, cleanup, err := prepareMancuernaLocalOnly()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if server.Addr != "127.0.0.1:18883" || server.ReadHeaderTimeout == 0 || server.WriteTimeout == 0 {
		t.Fatal("local listener broadened or lacks limits")
	}
	router, ok := server.Handler.(*gin.Engine)
	expected := expectedMancuernaRoutes()
	if !ok || len(router.Routes()) != len(expected) {
		t.Fatal("local server mounted unexpected routes")
	}
	for _, route := range router.Routes() {
		if _, ok := expected[route.Method+" "+route.Path]; !ok {
			t.Fatal("local server mounted unexpected route", route.Method, route.Path)
		}
	}
	for _, path := range []string{"/api/mancuerna/v1/me", "/api/v1/auth/login", "/health"} {
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if path == "/api/mancuerna/v1/me" && response.Code != 401 {
			t.Fatal("personal route not protected")
		}
		if path != "/api/mancuerna/v1/me" && response.Code != 404 {
			t.Fatal("administrative route was mounted")
		}
	}
	provider, err := mancuernaProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	status, err := provider.(*verify.LocalVerifyGuard).Status(context.Background())
	if err != nil || status.StartsReserved != 0 {
		t.Fatal("startup spent real-send reservation")
	}
	// No ListenAndServe call: this validates the actual configured server and
	// restricted database integration without opening a listener or sending OTP.
}
