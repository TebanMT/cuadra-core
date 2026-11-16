//go:build server && integration

package repositories

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestPersonalMiddlewarePostgresPreservesMixedScopesForUoW(t *testing.T) {
	// Validate both real restricted pools first, then close them. Auth allows
	// account+attempt during finalization, so its UoW alone cannot protect a
	// JWT route. The middleware must reject before either handler or SQL.
	uows := map[string]shared.UnitOfWork{}
	for name, role := range map[string]shared.PersonalRole{"MANCUERNA_TEST_DATA_DSN": shared.PersonalDataRole, "MANCUERNA_TEST_AUTH_DSN": shared.PersonalAuthRole} {
		db := profileTestPool(t, name)
		uow, err := shared.NewPersonalPostgresUnitOfWork(db, role)
		if err != nil {
			t.Fatal(err)
		}
		pool, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		if err = pool.Close(); err != nil {
			t.Fatal(err)
		}
		uows[string(role)] = uow
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := crypto.NewAccessTokens("scope-test", key, map[string]ed25519.PublicKey{"scope-test": pub})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	owner := uuid.New()
	encoded, _, err := tokens.Issue(owner, uuid.New(), uuid.New(), now)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := personalMiddleware.NewPersonal(tokens, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	base := context.Background()
	other := uuid.New()
	gin.SetMode(gin.TestMode)
	for name, scope := range map[string]context.Context{
		"other account":         shared.WithPersonalAccount(base, other),
		"invalid account":       shared.WithPersonalAccount(base, uuid.Nil),
		"account switched back": shared.WithPersonalAccount(shared.WithPersonalAccount(base, other), owner),
		"gym":                   shared.WithTenant(base, uuid.New()),
		"invalid gym":           shared.WithTenant(base, uuid.Nil),
		"attempt":               shared.WithPersonalAttempt(base, uuid.New(), [32]byte{1}),
	} {
		for poolName, uow := range uows {
			t.Run(name+"/"+poolName, func(t *testing.T) {
				called := false
				r := gin.New()
				r.Use(middleware)
				r.GET("/me", func(c *gin.Context) {
					called = true
					_, _ = uow.Query(c.Request.Context())
					c.Status(http.StatusNoContent)
				})
				request := httptest.NewRequest("GET", "/me", nil).WithContext(scope)
				request.Header.Set("Authorization", "Bearer "+encoded)
				response := httptest.NewRecorder()
				r.ServeHTTP(response, request)
				if called || response.Code != http.StatusUnauthorized || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("mixed scope reached authenticated handler")
				}
			})
		}
	}
}
