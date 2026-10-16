//go:build server && integration

package app_test

import (
	"context"
	"errors"
	gyms "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	app "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	domain "github.com/cuadra/cuadra-core/src/modules/notifications/domain"
	ne "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	n "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	repo "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/whatsapp"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countedProvider struct {
	*whatsapp.MockProvider
	calls atomic.Int32
	fail  atomic.Bool
}

func (p *countedProvider) SendTemplate(ctx context.Context, cfg domain.GymWhatsAppConfig, to, key string, vars map[string]string) (string, error) {
	p.calls.Add(1)
	if p.fail.Load() {
		return "", ne.ErrSendFailedRetry
	}
	return p.MockProvider.SendTemplate(ctx, cfg, to, key, vars)
}
func seedHeld(t *testing.T, db *gorm.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	gym, user := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO gyms(id,gym_id,name,country,timezone) VALUES(?,?,'Prueba retenidos','MX','America/Mexico_City')`, gym, gym).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO users(id,gym_id,email,password_hash,full_name,phone,role) VALUES(?,?,?,'test','Dueño prueba','+525512345678','owner')`, user, gym, user.String()+"@example.test").Error; err != nil {
		t.Fatal(err)
	}
	return gym, user
}
func TestHeldPostgresConcurrentRecoveryProviderAndRetention(t *testing.T) {
	db := testutil.OpenPostgres(t)
	gym, user := seedHeld(t, db)
	other, _ := seedHeld(t, db)
	ctx := shared.WithTenant(context.Background(), gym)
	uow := shared.NewPostgresUnitOfWork(db)
	nr := repo.NewNotificationPostgresRepository()
	tr := repo.NewTemplateOverridePostgresRepository()
	opt := repo.NewOptOutPostgresRepository()
	uc := app.NewHeldNotifications(nr, nr, tr, opt, uow, audit.NewPostgresRecorder())
	provider := &countedProvider{MockProvider: whatsapp.NewMockProvider()}
	dispatcher := app.NewDispatchNotification(nr, tr, gyms.NewGymPostgresRepository(), provider, nil, uow)
	dispatcher.Held = uc
	dispatcher.OptOut = opt
	makeMsg := func(key string) *n.Notification {
		msg, err := n.New(uuid.New(), gym, user, n.ChannelWhatsApp, key, n.RecipientUser, "+525512345678", map[string]string{"member_first_name": "Prueba", "gym_name": "Pruebas", "message": "Aviso revisado"}, time.Now(), time.Now().AddDate(0, 0, -35), nil)
		if err != nil {
			t.Fatal(err)
		}
		msg.MarkHeld("Llegó tarde", time.Now())
		if err = uow.Command(ctx, func(tx shared.Transaction) error { _, e := nr.Create(tx, msg); return e }); err != nil {
			t.Fatal(err)
		}
		return msg
	}
	msg := makeMsg("broadcast_freeform")
	in := app.HeldInput{GymID: gym, ID: msg.ID, ActorID: user, Version: msg.Version, Action: "send"}
	if _, err := uc.Execute(shared.WithTenant(context.Background(), other), app.HeldInput{GymID: other, ID: msg.ID, ActorID: user, Version: msg.Version, Action: "send"}); !errors.Is(err, ne.ErrHeldConflict) {
		t.Fatal("tenant leak", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := uc.Execute(ctx, in); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	now := time.Now().Add(time.Second)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := dispatcher.Tick(ctx, now); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls=%d", provider.calls.Load())
	}
	if _, err := uc.Execute(ctx, in); err != nil {
		t.Fatal("lost response replay", err)
	}
	if _, err := dispatcher.Tick(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 1 {
		t.Fatal("replayed send")
	}
	var stored struct {
		Status     string
		CreatedAt  time.Time
		RecoveryAt *time.Time
	}
	db.Table("notification_queue").Where("id=?", msg.ID).Scan(&stored)
	if stored.Status != "sent" || stored.RecoveryAt == nil || time.Since(stored.CreatedAt) < 30*24*time.Hour {
		t.Fatalf("freshness/event time %+v", stored)
	}
	// Ambiguous provider failures are never retried, including a manual failed retry.
	uncertain := makeMsg("broadcast_freeform")
	provider.fail.Store(true)
	_, err := uc.Execute(ctx, app.HeldInput{GymID: gym, ID: uncertain.ID, ActorID: user, Version: uncertain.Version, Action: "send"})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.Tick(ctx, time.Now().Add(time.Second))
	dispatcher.Tick(ctx, time.Now().Add(10*time.Minute))
	if provider.calls.Load() != 2 {
		t.Fatal("ambiguous result retried")
	}
	if err = app.NewRetryNotification(nr, uow, audit.NewPostgresRecorder()).Execute(ctx, app.RetryNotificationInput{GymID: gym, NotificationID: uncertain.ID, ActorUserID: user}); err == nil {
		t.Fatal("unsafe failed retry")
	}
	// A provider restriction introduced AFTER release is checked again at send time.
	opted := makeMsg("broadcast_freeform")
	uc.Execute(ctx, app.HeldInput{GymID: gym, ID: opted.ID, ActorID: user, Version: opted.Version, Action: "send"})
	uow.Command(ctx, func(tx shared.Transaction) error { return opt.SetOptedOut(tx, "+525512345678", time.Now()) })
	dispatcher.Tick(ctx, time.Now().Add(time.Second))
	if provider.calls.Load() != 2 {
		t.Fatal("opt-out bypass")
	}
	uow.Command(ctx, func(tx shared.Transaction) error { return opt.ClearOptOut(tx, "+525512345678") })
	staleSecret := makeMsg("operator_welcome_pin")
	if _, err = uc.Execute(ctx, app.HeldInput{GymID: gym, ID: staleSecret.ID, ActorID: user, Version: staleSecret.Version, Action: "send"}); err == nil {
		t.Fatal("credential revived")
	}
	discard := app.HeldInput{GymID: gym, ID: staleSecret.ID, ActorID: user, Version: staleSecret.Version, Action: "discard"}
	if _, err = uc.Execute(ctx, discard); err != nil {
		t.Fatal(err)
	}
	if _, err = uc.Execute(ctx, discard); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 23; i++ {
		makeMsg("operator_welcome_pin")
	}
	rows, total, err := uc.List(ctx, gym, 1)
	if err != nil || len(rows) != 20 || total != 23 {
		t.Fatalf("pagination %d %d %v", len(rows), total, err)
	}
	rows, total, err = uc.List(ctx, gym, 2)
	if err != nil || len(rows) != 3 || total != 23 {
		t.Fatal("page 2")
	}
	rows, total, err = uc.List(shared.WithTenant(context.Background(), other), other, 1)
	if err != nil || total != 0 {
		t.Fatal("list tenant leak")
	}
	old := makeMsg("operator_welcome_pin")
	db.Exec(`UPDATE notification_queue SET updated_at=NOW()-INTERVAL '91 days' WHERE id=?`, old.ID)
	uc.RetentionDays = 90
	count, err := uc.Cleanup(ctx, time.Now())
	if err != nil || count != 1 {
		t.Fatalf("cleanup=%d %v", count, err)
	}
	var archived int64
	db.Table("notification_queue").Where("id=? AND deleted_at IS NOT NULL AND status='cancelled'", old.ID).Count(&archived)
	if archived != 1 {
		t.Fatal("not soft archived")
	}
	var actions int64
	db.Table("audit_log").Where("entity_id=? AND action='held_send'", msg.ID).Count(&actions)
	if actions != 1 {
		t.Fatal("duplicate audit", actions)
	}
}
