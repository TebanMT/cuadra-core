//go:build server && integration

package app_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	subApp "github.com/cuadra/cuadra-core/src/modules/subscriptions/app"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	subDB "github.com/cuadra/cuadra-core/src/modules/subscriptions/infraestructure/db"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
	"github.com/google/uuid"
)

func TestLaunchConcurrentPaymentsCapAndRenewals(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	var before int64
	db.Table("launch_benefits").Count(&before)
	if before != 0 {
		t.Fatal("requires isolated empty launch ledger")
	}
	ids := make([]uuid.UUID, 60)
	for i := range ids {
		ids[i] = seedTrialGymWithSyncRow(t, db)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM launch_benefits WHERE gym_id IN ?`, ids) })
	uc := subApp.NewRecordEvent(subDB.NewEventPostgresRepository(), gymRepo.NewGymPostgresRepository(), sharedDomain.NewPostgresUnitOfWork(db), noopAudit{})
	uc.Launch = subDB.NewLaunchPostgresRepository()
	uc.SyncTouch = syncpkg.NewPostgresStore()
	amount := 799.0
	now := time.Now().UTC()
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			_, err := uc.Execute(ctx, subApp.RecordEventInput{GymID: id, Provider: subDomain.ProviderStripe, Type: subDomain.EventActivated, ExternalID: "paid-" + id.String(), Plan: gymDomain.PlanStandardMonthly, ConfirmedPayment: true, Amount: &amount, OccurredAt: now})
			errs <- err
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := db.Table("launch_benefits").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 55 {
		t.Fatalf("grants=%d", count)
	}
	var winner uuid.UUID
	if err := db.Raw(`SELECT gym_id FROM launch_benefits ORDER BY slot LIMIT 1`).Row().Scan(&winner); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		_, err := uc.Execute(ctx, subApp.RecordEventInput{GymID: id, Provider: subDomain.ProviderStripe, Type: subDomain.EventRenewed, ExternalID: "renewed-" + id.String(), Plan: gymDomain.PlanStandardMonthly, ConfirmedPayment: true, Amount: &amount, OccurredAt: now.Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
	}
	db.Table("launch_benefits").Count(&count)
	if count != 55 {
		t.Fatal("renewal consumed an extra slot")
	}
	uc.Launch = nil // the launch offer is closed, earned rights must survive.
	end := now.Add(-time.Hour)
	if _, err := uc.Execute(ctx, subApp.RecordEventInput{GymID: winner, Provider: subDomain.ProviderManual, Type: subDomain.EventCancelled, ExternalID: "cancel-" + winner.String(), OccurredAt: now.Add(time.Hour), PeriodEndsAt: &end}); err != nil {
		t.Fatal(err)
	}
	tx, _ := uc.UoW.Query(ctx)
	g, err := uc.Gyms.GetByID(tx, winner)
	if err != nil {
		t.Fatal(err)
	}
	if !g.AllFeaturesAccess || g.SubscriptionPlan != gymDomain.PlanStandardMonthly || g.HasActiveAccess(now.Add(2*time.Hour)) {
		t.Fatalf("wrong retained benefit: %+v", g)
	}
	remaining, err := subDB.NewLaunchPostgresRepository().Remaining(tx)
	if err != nil || remaining != 0 {
		t.Fatalf("remaining=%d err=%v", remaining, err)
	}
}

type failingLaunchTouch struct{}

func (failingLaunchTouch) TouchGym(context.Context, sharedDomain.Transaction, uuid.UUID) error {
	return errors.New("injected sync failure")
}
func TestLaunchGrantRollbackAndDelayedPayment(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	id := seedTrialGymWithSyncRow(t, db)
	t.Cleanup(func() { db.Exec(`DELETE FROM launch_benefits WHERE gym_id=?`, id) })
	uc := subApp.NewRecordEvent(subDB.NewEventPostgresRepository(), gymRepo.NewGymPostgresRepository(), sharedDomain.NewPostgresUnitOfWork(db), noopAudit{})
	uc.Launch = subDB.NewLaunchPostgresRepository()
	uc.SyncTouch = failingLaunchTouch{}
	amount := 799.0
	now := time.Now().UTC()
	paid := subApp.RecordEventInput{GymID: id, Provider: subDomain.ProviderStripe, Type: subDomain.EventActivated, ExternalID: "payment-" + id.String(), Plan: gymDomain.PlanStandardMonthly, ConfirmedPayment: true, Amount: &amount, OccurredAt: now}
	if _, err := uc.Execute(ctx, paid); err == nil {
		t.Fatal("expected rollback")
	}
	var n int64
	db.Table("launch_benefits").Where("gym_id=?", id).Count(&n)
	if n != 0 {
		t.Fatal("grant survived failed sync")
	}
	db.Table("subscription_events").Where("gym_id=?", id).Count(&n)
	if n != 0 {
		t.Fatal("payment survived failed sync")
	}
	uc.SyncTouch = syncpkg.NewPostgresStore()
	// A later cancellation may arrive before the successful-payment webhook.
	cancel := paid
	cancel.Type = subDomain.EventCancelled
	cancel.ExternalID = "cancel-" + id.String()
	cancel.OccurredAt = now.Add(time.Hour)
	cancel.ConfirmedPayment = false
	cancel.Amount = nil
	if _, err := uc.Execute(ctx, cancel); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.Execute(ctx, paid); err != nil {
		t.Fatal(err)
	}
	tx, _ := uc.UoW.Query(ctx)
	g, err := uc.Gyms.GetByID(tx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !g.AllFeaturesAccess || g.SubscriptionStatus != gymDomain.StatusCancelled {
		t.Fatal("delayed charge lost grant or reactivated subscription")
	}
	if out, err := uc.Execute(ctx, paid); err != nil || out.Applied {
		t.Fatal("retry was not idempotent", err)
	}
}

func TestLaunchBackfillUsesConfirmedPaymentsOnly(t *testing.T) {
	db := integrationDB(t)
	tx := db.Begin()
	defer tx.Rollback()
	schema := "launch_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	for _, q := range []string{`CREATE SCHEMA ` + schema, `SET LOCAL search_path TO ` + schema,
		`CREATE TABLE gyms(id UUID PRIMARY KEY,all_features_access BOOLEAN DEFAULT FALSE,version INTEGER DEFAULT 1,updated_at TIMESTAMPTZ DEFAULT NOW())`,
		`CREATE TABLE subscription_events(gym_id UUID,provider TEXT,type TEXT,amount NUMERIC,plan TEXT,occurred_at TIMESTAMPTZ,raw_payload JSONB)`,
		`CREATE TABLE sync_entities(gym_id UUID,entity_id UUID,entity_type TEXT,version INTEGER,server_updated_at TIMESTAMPTZ)`,
		`CREATE TABLE _migrations(version INTEGER,name TEXT,applied_at TIMESTAMPTZ)`} {
		if err := tx.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 60; i++ {
		id := uuid.New()
		if err := tx.Exec(`INSERT INTO gyms(id) VALUES(?)`, id).Error; err != nil {
			t.Fatal(err)
		}
		if err := tx.Exec(`INSERT INTO subscription_events VALUES(?,'manual','activated',799,'standard_monthly',NOW()+(? * INTERVAL '1 second'),'{}')`, id, i).Error; err != nil {
			t.Fatal(err)
		}
	}
	unpaid := uuid.New()
	tx.Exec(`INSERT INTO gyms(id) VALUES(?)`, unpaid)
	tx.Exec(`INSERT INTO subscription_events VALUES(?,'stripe','activated',799,'standard_monthly',NOW()-INTERVAL '1 year','{"event":"customer.subscription.created"}')`, unpaid)
	sql, err := os.ReadFile("../../../../db_migrations/postgres/053_launch_benefits.sql")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(strings.ReplaceAll(string(sql), "BEGIN;", ""), "COMMIT;", "")
	if err := tx.Exec(script).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	tx.Table("launch_benefits").Count(&count)
	if count != 50 {
		t.Fatalf("backfill=%d", count)
	}
	if err := tx.Exec(`CREATE TEMP TABLE launch_before_upgrade ON COMMIT DROP AS SELECT * FROM launch_benefits`).Error; err != nil {
		t.Fatal(err)
	}
	upgrade, err := os.ReadFile("../../../../db_migrations/postgres/054_launch_limit_55.sql")
	if err != nil {
		t.Fatal(err)
	}
	upgradeScript := strings.ReplaceAll(strings.ReplaceAll(string(upgrade), "BEGIN;", ""), "COMMIT;", "")
	if err := tx.Exec(upgradeScript).Error; err != nil {
		t.Fatal(err)
	}
	tx.Table("launch_benefits").Count(&count)
	if count != 55 {
		t.Fatalf("upgraded backfill=%d, want 55", count)
	}
	var changed int64
	if err := tx.Raw(`SELECT COUNT(*) FROM launch_before_upgrade old LEFT JOIN launch_benefits b ON b.gym_id=old.gym_id
 WHERE b.gym_id IS NULL OR b.slot<>old.slot OR b.first_paid_at<>old.first_paid_at OR b.created_at<>old.created_at`).Scan(&changed).Error; err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatal("upgrade changed existing benefits")
	}
	tx.Table("gyms").Where("all_features_access=TRUE").Count(&count)
	if count != 55 {
		t.Fatalf("granted gyms=%d, want 55", count)
	}
	var grant bool
	tx.Raw(`SELECT all_features_access FROM gyms WHERE id=?`, unpaid).Scan(&grant)
	if grant {
		t.Fatal("unpaid creation consumed launch place")
	}
}

func TestLaunchUnpaidEventsDoNotConsumeSlots(t *testing.T) {
	db := integrationDB(t)
	id := seedTrialGymWithSyncRow(t, db)
	t.Cleanup(func() { db.Exec(`DELETE FROM launch_benefits WHERE gym_id=?`, id) })
	uc := subApp.NewRecordEvent(subDB.NewEventPostgresRepository(), gymRepo.NewGymPostgresRepository(), sharedDomain.NewPostgresUnitOfWork(db), noopAudit{})
	uc.Launch = subDB.NewLaunchPostgresRepository()
	amount := 799.0
	zero := 0.0
	now := time.Now().UTC()
	for i, in := range []subApp.RecordEventInput{
		{Type: subDomain.EventActivated, Amount: &amount}, // subscription created, not charged
		{Type: subDomain.EventRenewed, Amount: &zero, ConfirmedPayment: true},
		{Type: subDomain.EventVoucherEmitted, Amount: &amount, ConfirmedPayment: true},
		{Type: subDomain.EventPastDue, Amount: &amount, ConfirmedPayment: true},
	} {
		in.GymID = id
		in.Provider = subDomain.ProviderStripe
		in.ExternalID = uuid.NewString()
		in.Plan = gymDomain.PlanStandardMonthly
		in.OccurredAt = now.Add(time.Duration(i) * time.Second)
		if _, err := uc.Execute(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	var n int64
	db.Table("launch_benefits").Where("gym_id=?", id).Count(&n)
	if n != 0 {
		t.Fatal("unpaid event consumed place")
	}
}
