//go:build server && integration

package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	gymRepoPg "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// El gate deja pasar si el lookup del gym falla (fail-open), así que una
// consulta que RLS no deja ver apagaría la solo lectura sin que nadie lo
// note. Esta prueba corre el lookup con un rol sin BYPASSRLS, como el server.
func TestIntegration_CloudTrialReadOnly_SeesGymUnderRLS(t *testing.T) {
	admin := testutil.OpenPostgres(t)
	now := time.Now().UTC()
	readOnly := seedCancelledTrial(t, admin, now.Add(-time.Hour))
	inGrace := seedCancelledTrial(t, admin, now.Add(48*time.Hour))

	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("test-secret")
	r := gin.New()
	r.Use(middleware.EnforceTrialReadOnly(tokens, gymRepoPg.NewGymPostgresRepository(), tenantAppUoW(t, admin)))
	r.POST("/api/v1/members", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	for gym, want := range map[uuid.UUID]int{readOnly: http.StatusPaymentRequired, inGrace: http.StatusOK} {
		token, err := tokens.GenerateAccessToken(uuid.New(), gym, "owner")
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/members", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("gym %s: POST /api/v1/members = %d, want %d (body=%s)", gym, w.Code, want, w.Body.String())
		}
	}
}

func seedCancelledTrial(t *testing.T, db *gorm.DB, graceEndsAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone,
		                  subscription_plan, subscription_status, trial_ends_at, subscription_ends_at)
		VALUES (?, ?, 1, NOW(), NOW(), 'Read-only Gate Test Gym', 'MX', 'America/Mexico_City', 'trial', 'cancelled', NOW() - INTERVAL '4 days', ?)`,
		id, id, graceEndsAt).Error; err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM cash_drawers WHERE gym_id = ?`, id)
		db.Exec(`DELETE FROM gyms WHERE id = ?`, id)
	})
	return id
}

// tenantAppUoW abre el UoW por tenant con un rol sin BYPASSRLS ni propiedad
// de tablas, igual que el server en producción.
func tenantAppUoW(t *testing.T, admin *gorm.DB) sharedDomain.UnitOfWork {
	t.Helper()
	role := "tinta_gate_" + uuid.New().String()[:8]
	password := uuid.NewString()
	if err := admin.Exec(`CREATE ROLE "` + role + `" LOGIN PASSWORD '` + password + `' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`DROP OWNED BY "` + role + `"`); admin.Exec(`DROP ROLE "` + role + `"`) })
	for _, sql := range []string{
		`GRANT USAGE ON SCHEMA public,tinta_private TO "` + role + `"`,
		`GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO "` + role + `"`,
		`GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO "` + role + `"`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA tinta_private TO "` + role + `"`,
	} {
		if err := admin.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/cuadra?sslmode=disable"
	}
	address, err := url.Parse(dsn)
	if err != nil || address.Host == "" {
		t.Fatal("integration DATABASE_URL must be a URL")
	}
	address.User = url.UserPassword(role, password)
	app, err := gorm.Open(postgres.Open(address.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := app.DB()
	t.Cleanup(func() { pool.Close() })
	uow, err := sharedDomain.NewTenantPostgresUnitOfWork(app)
	if err != nil {
		t.Fatal(err)
	}
	return uow
}
