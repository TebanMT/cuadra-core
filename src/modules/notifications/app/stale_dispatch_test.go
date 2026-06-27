package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notification "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Test DB-agnóstico de la supresión de mensajes stale (Fase 1). El dispatcher
// real corre sólo en el cloud (Postgres), donde `held` sí existe; acá usamos un
// repo in-memory para verificar la DECISIÓN —stale ⇒ no se envía, queda held—
// sin depender de ningún CHECK de DB. Para un mensaje stale, dispatchOne corta
// ANTES de tocar el provider/gym/template, así que esos pueden ir nil.

type fakeTx struct{}

func (fakeTx) Execute(fn func(tx sharedDomain.Transaction) error) error { return fn(fakeTx{}) }

type fakeUoW struct{}

func (fakeUoW) Begin(context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Commit(sharedDomain.Transaction) error                   { return nil }
func (fakeUoW) Rollback(sharedDomain.Transaction) error                 { return nil }
func (fakeUoW) Query(context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Command(_ context.Context, fn func(tx sharedDomain.Transaction) error) error {
	return fn(fakeTx{})
}

// fakeNotifRepo: store in-memory que acepta cualquier status (sin CHECK).
type fakeNotifRepo struct {
	rows map[uuid.UUID]*notification.Notification
}

func (f *fakeNotifRepo) LeasePending(_ sharedDomain.Transaction, now time.Time, _ int) ([]*notification.Notification, error) {
	out := []*notification.Notification{}
	for _, n := range f.rows {
		if n.Status == notification.StatusPending && !n.ScheduledFor.After(now) {
			out = append(out, n)
		}
	}
	return out, nil
}
func (f *fakeNotifRepo) GetByID(_ sharedDomain.Transaction, id uuid.UUID) (*notification.Notification, error) {
	return f.rows[id], nil
}
func (f *fakeNotifRepo) Update(_ sharedDomain.Transaction, n *notification.Notification) (*notification.Notification, error) {
	f.rows[n.ID] = n
	return n, nil
}

// stubs — no se tocan en el path de supresión.
func (f *fakeNotifRepo) Create(_ sharedDomain.Transaction, n *notification.Notification) (*notification.Notification, error) {
	return n, nil
}
func (f *fakeNotifRepo) GetByIdempotencyKey(sharedDomain.Transaction, uuid.UUID, string) (*notification.Notification, error) {
	return nil, nil
}
func (f *fakeNotifRepo) GetByProviderMessageID(sharedDomain.Transaction, string) (*notification.Notification, error) {
	return nil, nil
}
func (f *fakeNotifRepo) ListByGym(sharedDomain.Transaction, uuid.UUID, string, int, int) ([]*notification.Notification, int, error) {
	return nil, 0, nil
}
func (f *fakeNotifRepo) ChannelStats(sharedDomain.Transaction, uuid.UUID, string, time.Time) (notiRepo.NotificationStats, error) {
	return notiRepo.NotificationStats{}, nil
}
func (f *fakeNotifRepo) LastError(sharedDomain.Transaction, uuid.UUID, string) (string, *time.Time, error) {
	return "", nil, nil
}

func TestDispatch_StaleMessageHeldNotSent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	id := uuid.New()
	repo := &fakeNotifRepo{rows: map[uuid.UUID]*notification.Notification{
		id: {
			ID:           id,
			GymID:        uuid.New(),
			Channel:      notification.ChannelWhatsApp,
			TemplateKey:  "receipt_membership", // TTL 1d
			Status:       notification.StatusPending,
			ScheduledFor: now,                          // leasable
			CreatedAt:    now.Add(-10 * 24 * time.Hour), // 10 días viejo → stale
		},
	}}

	// templates/gyms/whatsapp/email = nil: el stale corta antes de usarlos.
	d := notiApp.NewDispatchNotification(repo, nil, nil, nil, nil, fakeUoW{})
	sent, err := d.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if sent != 0 {
		t.Errorf("un mensaje stale NO debe enviarse, got sent=%d", sent)
	}
	if got := repo.rows[id]; got.Status != notification.StatusHeld {
		t.Errorf("status = %q, want held", got.Status)
	}
	if got := repo.rows[id]; got.ErrorMessage == nil || *got.ErrorMessage == "" {
		t.Errorf("la razón del held debería quedar en error_message")
	}
}

func TestDispatch_FreshMessageNotHeld(t *testing.T) {
	// Un mensaje fresco (dentro del TTL) NO se retiene: pasa la supresión y
	// sigue al envío. Con whatsapp=nil, dispatchWhatsApp marca failed (no held),
	// lo que confirma que NO entró al path de stale.
	now := time.Unix(1_700_000_000, 0).UTC()
	id := uuid.New()
	repo := &fakeNotifRepo{rows: map[uuid.UUID]*notification.Notification{
		id: {
			ID:           id,
			GymID:        uuid.New(),
			Channel:      notification.ChannelWhatsApp,
			TemplateKey:  "receipt_membership",
			Status:       notification.StatusPending,
			ScheduledFor: now,
			CreatedAt:    now.Add(-2 * time.Hour), // fresco (< 1d)
		},
	}}
	d := notiApp.NewDispatchNotification(repo, nil, nil, nil, nil, fakeUoW{})
	if _, err := d.Tick(context.Background(), now); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := repo.rows[id]; got.Status == notification.StatusHeld {
		t.Errorf("un mensaje fresco NO debería quedar held")
	}
}
