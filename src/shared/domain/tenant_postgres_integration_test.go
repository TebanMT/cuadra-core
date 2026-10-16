//go:build server && integration

package domain_test

import (
	"context"
	"fmt"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	domain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestTenantRLSRuntimePoolAndFailClosed(t *testing.T) {
	admin := testutil.OpenPostgres(t)
	a, b := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{a, b} {
		if err := admin.Exec(`INSERT INTO gyms(id,gym_id) VALUES(?,?)`, id, id).Error; err != nil {
			t.Fatal(err)
		}
		if err := admin.Exec(`INSERT INTO products(id,gym_id,name,price,stock) VALUES(?,?,?,10,0)`, uuid.New(), id, "Producto").Error; err != nil {
			t.Fatal(err)
		}
	}
	role := "tinta_rls_" + uuid.New().String()[:8]
	password := uuid.NewString()
	if err := admin.Exec(`CREATE ROLE "` + role + `" LOGIN PASSWORD '` + password + `' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`DROP OWNED BY "` + role + `"`); admin.Exec(`DROP ROLE "` + role + `"`) })
	for _, sql := range []string{`GRANT USAGE ON SCHEMA public,tinta_private TO "` + role + `"`, `GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO "` + role + `"`, `GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA tinta_private TO "` + role + `"`} {
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
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	t.Cleanup(func() { pool.Close() })
	uow, err := domain.NewTenantPostgresUnitOfWork(app)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.NewTenantPostgresUnitOfWork(admin); err == nil {
		t.Fatal("server accepts bypass role")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	read := func(ctx context.Context, want int) {
		t.Helper()
		tx, err := uow.Query(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer domain.CloseQuery(tx)
		var n int64
		if err := tx.(*domain.GormTransaction).Tx.Raw(`SELECT COUNT(*) FROM products`).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n != int64(want) {
			t.Fatalf("unfiltered read got %d want %d", n, want)
		}
	}
	read(ctx, 0)
	for i := 0; i < 30; i++ {
		read(domain.WithTenant(ctx, a), 1)
		read(ctx, 0)
		read(domain.WithTenant(ctx, b), 1)
	}
	scoped := domain.WithTenant(ctx, a)
	if err := uow.Command(scoped, func(tx domain.Transaction) error {
		res := tx.(*domain.GormTransaction).Tx.Exec(`UPDATE products SET name='Ajeno' WHERE gym_id=?`, b)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 0 {
			t.Fatal("foreign update succeeded")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Command(scoped, func(tx domain.Transaction) error {
		return tx.(*domain.GormTransaction).Tx.Exec(`INSERT INTO products(id,gym_id,name,price,stock) VALUES(?,?,'Ajeno',10,0)`, uuid.New(), b).Error
	}); err == nil {
		t.Fatal("foreign insert succeeded")
	}
	if err := uow.Command(ctx, func(tx domain.Transaction) error {
		return tx.(*domain.GormTransaction).Tx.Exec(`INSERT INTO products(id,gym_id,name,price,stock) VALUES(?,?,'Sin contexto',10,0)`, uuid.New(), a).Error
	}); err == nil {
		t.Fatal("unscoped insert succeeded")
	}
	if _, err := uow.Query(domain.WithTenant(scoped, b)); err == nil {
		t.Fatal("nested tenant switch accepted")
	}
	sentinel := fmt.Errorf("rollback")
	if err := uow.Command(scoped, func(tx domain.Transaction) error { return sentinel }); err != sentinel {
		t.Fatal(err)
	}
	read(ctx, 0)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic swallowed")
			}
		}()
		_ = uow.Command(scoped, func(tx domain.Transaction) error { panic("rollback") })
	}()
	read(ctx, 0)
	if err := domain.ReadSnapshot(domain.WithTenant(ctx, b), uow, func(tx domain.Transaction) error {
		var n int64
		err := tx.(*domain.GormTransaction).Tx.Raw(`SELECT COUNT(*) FROM products`).Scan(&n).Error
		if n != 1 {
			t.Error("snapshot leaked")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	read(ctx, 0)
	visited := map[uuid.UUID]bool{}
	handled, _, err := domain.TenantWorker(ctx, uow, func(workerCtx context.Context) (int, error) {
		id := domain.TenantID(workerCtx)
		if id == uuid.Nil {
			return 0, fmt.Errorf("worker unscoped")
		}
		visited[id] = true
		tx, err := uow.Query(workerCtx)
		if err != nil {
			return 0, err
		}
		defer domain.CloseQuery(tx)
		var count int64
		err = tx.(*domain.GormTransaction).Tx.Raw(`SELECT COUNT(*) FROM gyms WHERE id<>?`, id).Scan(&count).Error
		if count != 0 {
			t.Error("worker sees another gym")
		}
		return 1, err
	})
	if !handled || err != nil || !visited[a] || !visited[b] {
		t.Fatalf("worker iteration: handled=%v err=%v", handled, err)
	}
	// The plan gate must release its read connection before the handler asks
	// for one. A one-connection pool makes this a deterministic deadlock check.
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(middleware.GymIDKey, a)
		c.Request = c.Request.WithContext(scoped)
		c.Next()
	})
	router.Use(middleware.RequirePlusPlan(gymRepo.NewGymPostgresRepository(), uow))
	if err := admin.Exec(`UPDATE gyms SET subscription_plan='plus_monthly' WHERE id=?`, a).Error; err != nil {
		t.Fatal(err)
	}
	router.GET("/pool", func(c *gin.Context) { read(c.Request.Context(), 1); c.Status(http.StatusNoContent) })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/pool", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("gate/handler failed: %d %s", response.Code, response.Body.String())
	}
	read(ctx, 0)
	if pool.Stats().InUse != 0 {
		t.Fatal("read transaction leaked a connection")
	}
}
