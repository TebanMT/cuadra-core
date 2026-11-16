//go:build server && integration

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	gymRepoPg "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	subApp "github.com/cuadra/cuadra-core/src/modules/subscriptions/app"
	subDB "github.com/cuadra/cuadra-core/src/modules/subscriptions/infraestructure/db"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

type noopAudit struct{}

func (noopAudit) Record(context.Context, sharedDomain.Transaction, audit.Entry) error { return nil }

type gymState struct {
	Plan        string     `gorm:"column:subscription_plan"`
	Status      string     `gorm:"column:subscription_status"`
	TrialEndsAt *time.Time `gorm:"column:trial_ends_at"`
}

func seedGym(t *testing.T, db *gorm.DB, plan, status string, trialEndsAt, graceEndsAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone,
		                  subscription_plan, subscription_status, trial_ends_at, subscription_ends_at)
		VALUES (?, ?, 1, NOW(), NOW(), 'Extend Trial Test Gym', 'MX', 'America/Mexico_City', ?, ?, ?, ?)`,
		id, id, plan, status, trialEndsAt, graceEndsAt).Error; err != nil {
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

// Reabre una prueba que el cloud ya dejó en solo lectura, avisa a las
// recepciones por sync y no extiende dos veces el mismo día.
func TestIntegration_ExtendTrial_ReopensReadOnlyTrialOnce(t *testing.T) {
	db := testutil.OpenPostgres(t)
	// Mediodía UTC de hoy: la repetición una hora después cae el mismo día.
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	gymID := seedGym(t, db, "trial", "cancelled", now.Add(-4*24*time.Hour), now.Add(-time.Hour))

	uow := sharedDomain.NewPostgresUnitOfWork(db)
	gyms := gymRepoPg.NewGymPostgresRepository()
	recorder := subApp.NewRecordEvent(subDB.NewEventPostgresRepository(), gyms, uow, noopAudit{})
	recorder.SyncTouch = syncpkg.NewPostgresStore()
	recorder.NowFunc = func() time.Time { return now }
	ctx := context.Background()
	in := extendInput{GymID: gymID, Days: 14, Note: "Piloto"}

	dry := in
	dry.DryRun = true
	preview, err := extendTrial(ctx, uow, gyms, recorder, dry, now)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	read := func() gymState {
		t.Helper()
		var s gymState
		if err := db.Raw(`SELECT subscription_plan, subscription_status, trial_ends_at FROM gyms WHERE id = ?`, gymID).Scan(&s).Error; err != nil {
			t.Fatalf("read gym: %v", err)
		}
		return s
	}
	if read().Status != "cancelled" {
		t.Fatal("dry-run must not write")
	}

	out, err := extendTrial(ctx, uow, gyms, recorder, in, now)
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if !out.Applied || !out.NewEnd.Equal(preview.NewEnd) {
		t.Fatalf("extend = %+v, want applied with the previewed end %s", out, preview.NewEnd)
	}
	after := read()
	if after.Status != "active" || after.TrialEndsAt == nil {
		t.Fatalf("gym after extend = %+v, want an active trial", after)
	}
	if got := after.TrialEndsAt.Sub(now); got < 14*24*time.Hour-time.Minute || got > 14*24*time.Hour+time.Minute {
		t.Fatalf("trial_ends_at = now+%s, want now+14d", got)
	}
	var touched int
	db.Raw(`SELECT COUNT(*) FROM sync_entities WHERE gym_id = ? AND entity_type = 'gyms' AND server_updated_at > NOW() - INTERVAL '1 minute'`, gymID).Scan(&touched)
	if touched != 1 {
		t.Fatal("the extension must reach the reception through sync")
	}

	again, err := extendTrial(ctx, uow, gyms, recorder, in, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again.Applied || !read().TrialEndsAt.Equal(*after.TrialEndsAt) {
		t.Fatal("re-running the same command the same day must not extend twice")
	}
	var events int
	db.Raw(`SELECT COUNT(*) FROM subscription_events WHERE gym_id = ? AND type = 'trial_extended'`, gymID).Scan(&events)
	if events != 1 {
		t.Fatalf("trial_extended events = %d, want 1", events)
	}
}

func TestIntegration_ExtendTrial_RefusesPaidPlan(t *testing.T) {
	db := testutil.OpenPostgres(t)
	now := time.Now().UTC()
	gymID := seedGym(t, db, "standard_monthly", "active", now.Add(-30*24*time.Hour), now.Add(20*24*time.Hour))
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	gyms := gymRepoPg.NewGymPostgresRepository()
	recorder := subApp.NewRecordEvent(subDB.NewEventPostgresRepository(), gyms, uow, noopAudit{})

	_, err := extendTrial(context.Background(), uow, gyms, recorder, extendInput{GymID: gymID, Days: 7}, now)
	if !errors.Is(err, errNotOnTrial) {
		t.Fatalf("err = %v, want errNotOnTrial", err)
	}
	var events int
	db.Raw(`SELECT COUNT(*) FROM subscription_events WHERE gym_id = ?`, gymID).Scan(&events)
	if events != 0 {
		t.Fatal("a refused extension must not record events")
	}
}
