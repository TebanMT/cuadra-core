//go:build sidecar

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	alertDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/alertconfig"
	notiRepoLite "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/repositories"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
)

// UC-040 DA-40.1 — owner toggles a config and the dispatch path respects it.
//
// We exercise three things end-to-end against a real SQLite + sync-queue
// fixture: the default merge view (no rows persisted, every key enabled),
// the upsert via UpdateOwnerAlert, and the gating in EnqueueOwnerAlert
// when the toggle is off.
func TestUC040_OwnerAlertsRespectConfigToggle(t *testing.T) {
	f := newFixture(t)
	recorder := audit.NewSQLiteRecorder()
	userRepo := usersRepoLite.NewUserSQLiteRepository()

	configRepo := notiRepoLite.NewAlertConfigSQLiteRepository()
	templateRepo := notiRepoLite.NewTemplateOverrideSQLiteRepository()
	updateUC := notiApp.NewUpdateOwnerAlert(configRepo, templateRepo, f.uow, recorder)
	listUC := notiApp.NewListOwnerAlerts(configRepo, templateRepo, f.uow)
	enqueueUC := notiApp.NewEnqueueOwnerAlert(f.notiRepo, f.gymRepo, userRepo, configRepo, f.uow)

	// 1. Defaults: every key reports enabled=true with no rows persisted.
	views, err := listUC.Execute(context.Background(), f.gymID)
	if err != nil {
		t.Fatalf("list defaults: %v", err)
	}
	if len(views) != len(alertDomain.Defaults()) {
		t.Fatalf("default view len = %d, want %d", len(views), len(alertDomain.Defaults()))
	}
	for _, v := range views {
		if !v.Enabled {
			t.Errorf("default for %s should be enabled", v.Key)
		}
	}

	// 2. Disable cash_close_diff. Persists an override row.
	off := false
	saved, err := updateUC.Execute(context.Background(), notiApp.UpdateOwnerAlertInput{
		GymID:       f.gymID,
		ActorUserID: f.ownerID,
		Key:         alertDomain.KeyCashCloseDiff,
		Enabled:     &off,
	})
	if err != nil {
		t.Fatalf("update toggle: %v", err)
	}
	if saved.Enabled {
		t.Errorf("expected enabled=false after toggle off")
	}
	if saved.Version != 1 {
		t.Errorf("first write should have version=1, got %d", saved.Version)
	}

	// 3. List now reflects the override.
	views, err = listUC.Execute(context.Background(), f.gymID)
	if err != nil {
		t.Fatalf("list after toggle: %v", err)
	}
	for _, v := range views {
		if v.Key == alertDomain.KeyCashCloseDiff && v.Enabled {
			t.Errorf("cash_close_diff should be disabled after toggle")
		}
	}

	// 4. EnqueueOwnerAlert short-circuits on the disabled key with
	//    SkippedReason="alert_disabled" and never creates a queue row.
	//    No need to set up WhatsApp connectivity — the config check runs
	//    before the gym/owner lookup.
	out, err := enqueueUC.Execute(context.Background(), notiApp.EnqueueOwnerAlertInput{
		GymID: f.gymID,
		Kind:  notiApp.OwnerAlertCashCloseDiff,
		Vars:  map[string]string{"diff_amount": "42.50"},
	})
	if err != nil {
		t.Fatalf("enqueue disabled: %v", err)
	}
	if !out.Skipped || out.SkippedReason != "alert_disabled" {
		t.Errorf("expected skipped=alert_disabled, got skipped=%v reason=%q", out.Skipped, out.SkippedReason)
	}
	if out.NotificationID != nil {
		t.Errorf("disabled key should not produce a notification id, got %v", *out.NotificationID)
	}

	// 5. Confirm no notification_queue row was inserted.
	tx, _ := f.uow.Query(context.Background())
	_, total, err := f.notiRepo.ListByGym(tx, f.gymID, "", 1, 50)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if total != 0 {
		t.Errorf("disabled toggle should leave queue empty, got %d rows", total)
	}
}

// TestUC040_VersionBumpsOnToggleChange verifies the per-row version
// counter increments only when the value actually changes — an idempotent
// PATCH from the FE shouldn't churn the version.
func TestUC040_VersionBumpsOnToggleChange(t *testing.T) {
	f := newFixture(t)
	recorder := audit.NewSQLiteRecorder()
	configRepo := notiRepoLite.NewAlertConfigSQLiteRepository()
	templateRepo := notiRepoLite.NewTemplateOverrideSQLiteRepository()
	updateUC := notiApp.NewUpdateOwnerAlert(configRepo, templateRepo, f.uow, recorder)

	off := false
	on := true
	v1, err := updateUC.Execute(context.Background(), notiApp.UpdateOwnerAlertInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Key: alertDomain.KeyLowStock, Enabled: &off,
	})
	if err != nil {
		t.Fatalf("first toggle: %v", err)
	}
	if v1.Version != 1 {
		t.Errorf("v1 version=%d want 1", v1.Version)
	}

	v2, err := updateUC.Execute(context.Background(), notiApp.UpdateOwnerAlertInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Key: alertDomain.KeyLowStock, Enabled: &off,
	})
	if err != nil {
		t.Fatalf("idempotent toggle: %v", err)
	}
	if v2.Version != 1 {
		t.Errorf("idempotent PATCH should keep version=1, got %d", v2.Version)
	}

	v3, err := updateUC.Execute(context.Background(), notiApp.UpdateOwnerAlertInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Key: alertDomain.KeyLowStock, Enabled: &on,
	})
	if err != nil {
		t.Fatalf("flip toggle: %v", err)
	}
	if v3.Version != 2 {
		t.Errorf("real change should bump to v2, got %d", v3.Version)
	}
}

// TestUC040_UnknownKeyRejected verifies stale FE state can't pollute the
// DB with non-canonical keys.
func TestUC040_UnknownKeyRejected(t *testing.T) {
	f := newFixture(t)
	recorder := audit.NewSQLiteRecorder()
	configRepo := notiRepoLite.NewAlertConfigSQLiteRepository()
	templateRepo := notiRepoLite.NewTemplateOverrideSQLiteRepository()
	updateUC := notiApp.NewUpdateOwnerAlert(configRepo, templateRepo, f.uow, recorder)

	off := false
	_, err := updateUC.Execute(context.Background(), notiApp.UpdateOwnerAlertInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Key: alertDomain.Key("not_a_real_key"), Enabled: &off,
	})
	if err == nil {
		t.Fatal("expected validation error for unknown key")
	}
}

func TestUC040_CashDiffAlertIsPerSessionAndResult(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Exec(`UPDATE users SET phone='+525500000000' WHERE id=?`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	userRepo := usersRepoLite.NewUserSQLiteRepository()
	enqueue := notiApp.NewEnqueueOwnerAlert(f.notiRepo, f.gymRepo, userRepo, notiRepoLite.NewAlertConfigSQLiteRepository(), f.uow)
	subscriber := notiApp.NewCashCloseAlertSubscriber(enqueue)
	day := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	ana, note := "Ana", "no había cambio"
	diff := func(v float64) *float64 { return &v }
	first, second := uuid.New(), uuid.New()
	ctx := context.Background()
	subscriber.OnCashCloseClosed(ctx, reportsApp.CashCloseClosedEvent{GymID: f.gymID, SessionID: first, CloseDate: day,
		Discrepancy: diff(-50), CountedByName: &ana, Reason: &note})
	// A retried request for the same count must not message the owner twice.
	subscriber.OnCashCloseClosed(ctx, reportsApp.CashCloseClosedEvent{GymID: f.gymID, SessionID: first, CloseDate: day,
		Discrepancy: diff(-50), CountedByName: &ana, Reason: &note})
	// A recount that changes the result is new information.
	subscriber.OnCashCloseClosed(ctx, reportsApp.CashCloseClosedEvent{GymID: f.gymID, SessionID: first, CloseDate: day,
		Discrepancy: diff(-20)})
	// A second session or drawer on the same day used to collide on the date key.
	subscriber.OnCashCloseClosed(ctx, reportsApp.CashCloseClosedEvent{GymID: f.gymID, SessionID: second, CloseDate: day,
		Discrepancy: diff(15)})
	subscriber.OnCashCloseClosed(ctx, reportsApp.CashCloseClosedEvent{GymID: f.gymID, SessionID: uuid.New(), CloseDate: day,
		Discrepancy: diff(0.004)})

	tx, err := f.uow.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, total, err := f.notiRepo.ListByGym(tx, f.gymID, "", 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("cash diff alerts = %d, want 3 (one per session and result)", total)
	}
	amounts := map[string]bool{}
	for _, row := range rows {
		if row.TemplateKey != "owner_alert_cash_close_diff" {
			t.Fatalf("unexpected template %q", row.TemplateKey)
		}
		amounts[row.Payload["diff_amount"]] = true
	}
	for _, want := range []string{
		"50.00 (faltante) · contó Ana · 12 nov · Nota: no había cambio",
		"20.00 (faltante) · 12 nov",
		"15.00 (sobrante) · 12 nov",
	} {
		if !amounts[want] {
			t.Errorf("missing alert %q in %v", want, amounts)
		}
	}
}
