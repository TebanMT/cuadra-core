// Package repository declares the persistence contracts for the
// notifications BC. Concrete impls live under infraestructure/db/repositories
// behind the `server` and `sidecar` build tags.
package repository

import (
	"time"

	"github.com/google/uuid"

	alertDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/alertconfig"
	eventDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/event"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// NotificationRepository — the `notification_queue` table.
type NotificationRepository interface {
	Create(tx sharedDomain.Transaction, n *notiDomain.Notification) (*notiDomain.Notification, error)
	Update(tx sharedDomain.Transaction, n *notiDomain.Notification) (*notiDomain.Notification, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*notiDomain.Notification, error)
	GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*notiDomain.Notification, error)
	GetByProviderMessageID(tx sharedDomain.Transaction, providerMessageID string) (*notiDomain.Notification, error)

	// LeasePending fetches up to `limit` pending rows whose scheduled_for is
	// in the past. Implementations SHOULD use SELECT ... FOR UPDATE SKIP
	// LOCKED (Postgres) so multiple workers don't race; SQLite serialises
	// transactions naturally so a plain SELECT is fine for the sidecar.
	LeasePending(tx sharedDomain.Transaction, now time.Time, limit int) ([]*notiDomain.Notification, error)

	// ListByGym is the debug listing endpoint (UC-040 dashboard).
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, statusFilter string, page, pageSize int) ([]*notiDomain.Notification, int, error)

	// ChannelStats agrega contadores del último período (since → now) para un
	// canal específico (whatsapp, email, …). Lo consume el handler de
	// /api/v1/gyms/me/whatsapp para llenar la card de stats sin tener que
	// proxy-ear al cloud — el sync ya trae notification_queue al sidecar
	// (ver SyncedTables), así que la agregación es un COUNT local.
	ChannelStats(tx sharedDomain.Transaction, gymID uuid.UUID, channel string, since time.Time) (NotificationStats, error)

	// LastError devuelve el error_message + failed_at del último envío
	// fallido del canal indicado. Se usa para mostrar "último error" en la
	// card de estado de WhatsApp. Devuelve ("", nil, nil) si no hay
	// fallidos — el caller renderiza "—" en ese caso.
	LastError(tx sharedDomain.Transaction, gymID uuid.UUID, channel string) (msg string, at *time.Time, err error)
}

// NotificationStats es el agregado que ChannelStats devuelve. Sent cuenta
// todos los rows del canal en el rango (incluye los pending y failed);
// Delivered cuenta los que llegaron a sent_at; Failed cuenta los que
// terminaron en failed_at. Pending = Sent − Delivered − Failed implícito.
type NotificationStats struct {
	Sent      int
	Delivered int
	Failed    int
}

// TemplateOverrideRepository — `notification_templates`. The default template
// library lives in code (`template.DefaultLibrary`); this table only holds
// per-gym overrides created via UC-038 DA-38.2.
type TemplateOverrideRepository interface {
	Upsert(tx sharedDomain.Transaction, o *tplDomain.Override) (*tplDomain.Override, error)
	GetByGymAndKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*tplDomain.Override, error)
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*tplDomain.Override, error)
}

// WhatsAppEventRepository — `whatsapp_events`. Append-only log of webhook
// payloads.
type WhatsAppEventRepository interface {
	Create(tx sharedDomain.Transaction, e *eventDomain.WhatsAppEvent) (*eventDomain.WhatsAppEvent, error)
	ListByNotification(tx sharedDomain.Transaction, notificationID uuid.UUID) ([]*eventDomain.WhatsAppEvent, error)
}

// AlertConfigRepository — `owner_alert_configs`. Defaults live in code
// (alertconfig.Defaults); rows exist only when the owner has flipped the
// toggle. GetByGymAndKey returns (nil, nil) when no override is present —
// callers fall back to alertconfig.LookupDefault.
type AlertConfigRepository interface {
	Upsert(tx sharedDomain.Transaction, c *alertDomain.Config) (*alertDomain.Config, error)
	GetByGymAndKey(tx sharedDomain.Transaction, gymID uuid.UUID, key alertDomain.Key) (*alertDomain.Config, error)
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*alertDomain.Config, error)
}
