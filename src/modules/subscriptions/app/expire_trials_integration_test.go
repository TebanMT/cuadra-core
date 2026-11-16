//go:build server && integration

package app_test

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	gymRepoPg "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	subApp "github.com/cuadra/cuadra-core/src/modules/subscriptions/app"
	subDB "github.com/cuadra/cuadra-core/src/modules/subscriptions/infraestructure/db"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// appRoleUoW abre el UoW por tenant con un rol sin BYPASSRLS, como el server
// en producción: el job recorre los gyms con ForEachTenant y cada consulta
// pasa por las políticas RLS.
func appRoleUoW(t *testing.T, admin *gorm.DB) sharedDomain.UnitOfWork {
	t.Helper()
	role := "tinta_trial_" + uuid.New().String()[:8]
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

func seedGym(t *testing.T, db *gorm.DB, plan string, trialEndsAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone,
		                  subscription_plan, subscription_status, trial_ends_at)
		VALUES (?, ?, 1, NOW(), NOW(), 'Trial Expiry Test Gym', 'MX', 'America/Mexico_City', ?, 'active', ?)`,
		id, id, plan, trialEndsAt).Error; err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	payload := `{"id":"` + id.String() + `","gym_id":"` + id.String() + `"}`
	if err := db.Exec(`
		INSERT INTO sync_entities (gym_id, entity_type, entity_id, version, payload, server_updated_at)
		VALUES (?, 'gyms', ?, 1, ?::jsonb, NOW() - INTERVAL '1 hour')`, id, id, payload).Error; err != nil {
		t.Fatalf("seed sync_entities: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM subscription_events WHERE gym_id = ?`, id)
		db.Exec(`DELETE FROM sync_entities WHERE gym_id = ?`, id)
		db.Exec(`DELETE FROM cash_drawers WHERE gym_id = ?`, id)
		db.Exec(`DELETE FROM gyms WHERE id = ?`, id)
	})
	return id
}

func TestIntegration_ExpireTrials_ClosesOnlyEndedTrialsUnderRLS(t *testing.T) {
	admin := integrationDB(t)
	now := time.Now().UTC()
	ended := seedGym(t, admin, "trial", now.Add(-2*time.Hour))
	running := seedGym(t, admin, "trial", now.Add(5*24*time.Hour))
	paid := seedGym(t, admin, "standard_monthly", now.Add(-10*24*time.Hour))

	uow := appRoleUoW(t, admin)
	recorder := subApp.NewRecordEvent(subDB.NewEventPostgresRepository(), gymRepoPg.NewGymPostgresRepository(), uow, noopAudit{})
	recorder.SyncTouch = syncpkg.NewPostgresStore()
	uc := subApp.NewExpireTrials(subDB.NewTrialExpiryPostgresReader(), recorder, uow)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := uc.Tick(ctx, now); err != nil {
		t.Fatalf("tick: %v", err)
	}

	type state struct {
		Plan    string     `gorm:"column:subscription_plan"`
		Status  string     `gorm:"column:subscription_status"`
		EndsAt  *time.Time `gorm:"column:subscription_ends_at"`
		Version int
	}
	read := func(id uuid.UUID) state {
		t.Helper()
		var s state
		if err := admin.Raw(`SELECT subscription_plan, subscription_status, subscription_ends_at, version FROM gyms WHERE id = ?`, id).Scan(&s).Error; err != nil {
			t.Fatalf("read gym: %v", err)
		}
		return s
	}
	events := func(id uuid.UUID) int {
		t.Helper()
		var n int
		admin.Raw(`SELECT COUNT(*) FROM subscription_events WHERE gym_id = ? AND type = 'cancelled'`, id).Scan(&n)
		return n
	}

	got := read(ended)
	if got.Plan != "trial" || got.Status != "cancelled" || got.EndsAt == nil {
		t.Fatalf("ended trial = %+v, want trial/cancelled with grace", got)
	}
	if d := got.EndsAt.Sub(now); d < 71*time.Hour || d > 73*time.Hour {
		t.Errorf("grace = %v, want ~72h", d)
	}
	if events(ended) != 1 {
		t.Errorf("ended trial events = %d, want 1", events(ended))
	}
	if v := syncJournalVersion(t, admin, ended); v < 2 {
		t.Errorf("journal version = %d, want bumped so the sidecar pulls the change", v)
	}
	for name, id := range map[string]uuid.UUID{"running trial": running, "paid plan": paid} {
		if s := read(id); s.Status != "active" || events(id) != 0 {
			t.Errorf("%s was touched: %+v events=%d", name, s, events(id))
		}
	}

	// Una segunda corrida no vuelve a cerrar ni a registrar nada.
	if _, err := uc.Tick(ctx, now.Add(time.Hour)); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if events(ended) != 1 {
		t.Errorf("events after replay = %d, want 1", events(ended))
	}
}
