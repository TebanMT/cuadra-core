package app

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain"
	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	notification "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// DispatchNotification is the worker that drains `notification_queue`. One
// goroutine wakes every `Interval` (default 5s), leases up to `BatchSize`
// pending rows, and dispatches them via the configured providers.
//
// Failure modes (mapped from provider errors):
//
//   - notiErrors.ErrSendFailedRetry — increment retry_count, push
//     scheduled_for forward with exponential backoff. Status stays pending.
//   - notiErrors.ErrSendFailedFinal — set status=failed, fail_at=NOW.
//   - any other err — treated as transient retry; first 3 attempts back off,
//     after that the row is failed permanently.
type DispatchNotification struct {
	Notifications notiRepo.NotificationRepository
	Templates     notiRepo.TemplateOverrideRepository
	Gyms          gymRepo.GymRepository
	WhatsApp      notiDomain.WhatsAppProvider
	Email         notiDomain.EmailProvider
	UoW           sharedDomain.UnitOfWork

	BatchSize  int
	MaxRetries int
}

// MaxRetries default — after 5 attempts the row is failed permanently.
const defaultMaxRetries = 5

func NewDispatchNotification(
	notifications notiRepo.NotificationRepository,
	templates notiRepo.TemplateOverrideRepository,
	gyms gymRepo.GymRepository,
	whatsapp notiDomain.WhatsAppProvider,
	email notiDomain.EmailProvider,
	uow sharedDomain.UnitOfWork,
) *DispatchNotification {
	return &DispatchNotification{
		Notifications: notifications,
		Templates:     templates,
		Gyms:          gyms,
		WhatsApp:      whatsapp,
		Email:         email,
		UoW:           uow,
		BatchSize:     20,
		MaxRetries:    defaultMaxRetries,
	}
}

// Tick processes one batch. Returns the number of rows successfully sent.
// Errors at the lease level (DB unreachable) are returned; per-message
// failures are recorded on the row and the loop continues.
func (uc *DispatchNotification) Tick(ctx context.Context, now time.Time) (int, error) {
	if uc.BatchSize <= 0 {
		uc.BatchSize = 20
	}

	// Lease + claim happens inside one Command tx — once we commit the
	// rows are off the queue (status=pending but scheduled_for in the
	// future, which the next leaser skips).
	var pending []*notification.Notification
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		rows, err := uc.Notifications.LeasePending(tx, now, uc.BatchSize)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// Push scheduled_for forward by `claimWindow` so concurrent workers
		// don't grab the same rows. We update inline; the actual provider
		// call is outside the tx.
		const claimWindow = 5 * time.Minute
		for _, n := range rows {
			n.ScheduledFor = now.Add(claimWindow)
			n.UpdatedAt = now
			if _, err := uc.Notifications.Update(tx, n); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}
		pending = rows
		return nil
	})
	if err != nil {
		return 0, err
	}

	sent := 0
	for _, n := range pending {
		if uc.dispatchOne(ctx, n, now) {
			sent++
		}
	}
	return sent, nil
}

// dispatchOne fires the provider call and persists the resulting state.
// Returns true on successful send.
func (uc *DispatchNotification) dispatchOne(ctx context.Context, n *notification.Notification, now time.Time) bool {
	switch n.Channel {
	case notification.ChannelWhatsApp:
		return uc.dispatchWhatsApp(ctx, n, now)
	case notification.ChannelEmail:
		return uc.dispatchEmail(ctx, n, now)
	case notification.ChannelInApp:
		// In-app delivery is just a flag flip — UI consumes via /notifications
		// listing. Nothing to call out to.
		return uc.markSent(ctx, n, "in_app", now)
	default:
		uc.markFailed(ctx, n, "canal desconocido", now, false)
		return false
	}
}

func (uc *DispatchNotification) dispatchWhatsApp(ctx context.Context, n *notification.Notification, now time.Time) bool {
	if uc.WhatsApp == nil {
		uc.markFailed(ctx, n, "whatsapp provider not configured", now, false)
		return false
	}

	var cfg notiDomain.GymWhatsAppConfig
	queryErr := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		gym, err := uc.Gyms.GetByID(tx, n.GymID)
		if err != nil {
			return err
		}
		if !gym.IsWhatsAppConnected() {
			return notiErrors.ErrNotConnected
		}
		cfg = notiDomain.GymWhatsAppConfig{GymID: gym.ID, Phone: *gym.WhatsAppBusinessPhone}
		return nil
	})
	if queryErr != nil {
		// gym-not-connected is final; everything else retryable. Si el
		// caller incluyó `fallback_email` en el payload, intentamos enviar
		// el mismo template por correo antes de descartar — esto da a los
		// gyms que aún no conectan WhatsApp un canal mínimo de
		// notificación (recordatorios de vencimiento, alertas) en lugar
		// de perder el mensaje en silencio.
		if errors.Is(queryErr, notiErrors.ErrNotConnected) {
			if uc.tryEmailFallback(ctx, n, now) {
				return true
			}
			uc.markFailed(ctx, n, "gym sin WhatsApp conectado", now, false)
		} else {
			uc.markFailed(ctx, n, queryErr.Error(), now, true)
		}
		return false
	}

	// Resolve template body — per-gym override wins over default.
	override := uc.lookupOverride(ctx, n.GymID, n.TemplateKey)
	if override != nil && !override.Enabled {
		uc.markFailed(ctx, n, "template deshabilitado por el gym", now, false)
		return false
	}

	providerMsgID, sendErr := uc.WhatsApp.SendTemplate(ctx, cfg, n.RecipientAddress, n.TemplateKey, n.Payload)
	if sendErr != nil {
		// Distinguish retry-vs-final.
		if errors.Is(sendErr, notiErrors.ErrSendFailedFinal) {
			uc.markFailed(ctx, n, sendErr.Error(), now, false)
			return false
		}
		uc.markFailed(ctx, n, sendErr.Error(), now, true)
		return false
	}

	return uc.markSent(ctx, n, providerMsgID, now)
}

// tryEmailFallback intenta enviar `n` por correo cuando el path WhatsApp
// no aplica (gym sin número conectado). El caller indica intención por
// payload["fallback_email"]: si está ausente, no hay fallback y el row
// se marca failed final como antes.
//
// Reusa el body del template WhatsApp como cuerpo del email — la mayoría
// de los templates son ChannelWhatsApp y duplicar la copy en una
// definición Email separada es ruido a esta escala. Cuando exista un
// volumen de gyms email-only suficiente para justificarlo, migrar a
// templates dedicados con Subject/Body distintos.
//
// Devuelve true si el email se envió (markSent ya invocado); false si
// no hay fallback o el envío falló (el caller marcará failed final).
func (uc *DispatchNotification) tryEmailFallback(ctx context.Context, n *notification.Notification, now time.Time) bool {
	if uc.Email == nil {
		return false
	}
	email := strings.TrimSpace(n.Payload["fallback_email"])
	if email == "" {
		return false
	}
	def := tplDomain.LookupDefault(n.TemplateKey)
	if def == nil {
		return false
	}
	body := def.Body
	if override := uc.lookupOverride(ctx, n.GymID, n.TemplateKey); override != nil {
		if !override.Enabled {
			return false
		}
		body = override.Body
	}
	rendered, err := tplDomain.Render(body, n.Payload)
	if err != nil {
		return false
	}
	subject := def.Subject
	if subject == "" {
		subject = humanise(n.TemplateKey)
	}
	providerMsgID, sendErr := uc.Email.SendTransactional(ctx, email, subject, rendered, n.TemplateKey)
	if sendErr != nil {
		return false
	}
	return uc.markSent(ctx, n, "email_fallback:"+providerMsgID, now)
}

func (uc *DispatchNotification) dispatchEmail(ctx context.Context, n *notification.Notification, now time.Time) bool {
	if uc.Email == nil {
		uc.markFailed(ctx, n, "email provider not configured", now, false)
		return false
	}
	def := tplDomain.LookupDefault(n.TemplateKey)
	if def == nil {
		uc.markFailed(ctx, n, "template not found", now, false)
		return false
	}
	body := def.Body
	if override := uc.lookupOverride(ctx, n.GymID, n.TemplateKey); override != nil && override.Enabled {
		body = override.Body
	}
	rendered, err := tplDomain.Render(body, n.Payload)
	if err != nil {
		uc.markFailed(ctx, n, err.Error(), now, false)
		return false
	}
	subject := def.Subject
	if subject == "" {
		subject = humanise(n.TemplateKey)
	}
	providerMsgID, sendErr := uc.Email.SendTransactional(ctx, n.RecipientAddress, subject, rendered, n.TemplateKey)
	if sendErr != nil {
		if errors.Is(sendErr, notiErrors.ErrSendFailedFinal) {
			uc.markFailed(ctx, n, sendErr.Error(), now, false)
			return false
		}
		uc.markFailed(ctx, n, sendErr.Error(), now, true)
		return false
	}
	return uc.markSent(ctx, n, providerMsgID, now)
}

func (uc *DispatchNotification) lookupOverride(ctx context.Context, gymID uuid.UUID, key string) *tplDomain.Override {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil
	}
	override, err := uc.Templates.GetByGymAndKey(tx, gymID, key)
	if err != nil {
		return nil
	}
	return override
}

func (uc *DispatchNotification) markSent(ctx context.Context, n *notification.Notification, providerMsgID string, now time.Time) bool {
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		fresh, err := uc.Notifications.GetByID(tx, n.ID)
		if err != nil {
			return err
		}
		if err := fresh.MarkSent(providerMsgID, now); err != nil {
			return err
		}
		_, err = uc.Notifications.Update(tx, fresh)
		return err
	})
	if err != nil {
		log.Printf("[notifications/dispatch] markSent failed id=%s err=%v", n.ID, err)
		return false
	}
	return true
}

func (uc *DispatchNotification) markFailed(ctx context.Context, n *notification.Notification, reason string, now time.Time, retry bool) {
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		fresh, err := uc.Notifications.GetByID(tx, n.ID)
		if err != nil {
			return err
		}
		max := uc.MaxRetries
		if max <= 0 {
			max = defaultMaxRetries
		}
		if retry && fresh.RetryCount+1 < max {
			next := now.Add(retryBackoff(fresh.RetryCount + 1))
			fresh.MarkFailedRetry(reason, next, now)
		} else {
			fresh.MarkFailedFinal(reason, now)
		}
		_, err = uc.Notifications.Update(tx, fresh)
		return err
	})
	if err != nil {
		log.Printf("[notifications/dispatch] markFailed failed id=%s err=%v", n.ID, err)
	}
}

// retryBackoff: 1m, 2m, 5m, 15m, 1h. Beyond the table we fail final.
func retryBackoff(retry int) time.Duration {
	switch retry {
	case 1:
		return time.Minute
	case 2:
		return 2 * time.Minute
	case 3:
		return 5 * time.Minute
	case 4:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}

// humanise turns "expiry_reminder_3d" into "Expiry Reminder 3d" for email
// subjects when the template doesn't carry one.
func humanise(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// Worker runs Tick every Interval until ctx is cancelled.
type Worker struct {
	uc       *DispatchNotification
	interval time.Duration

	stopOnce sync.Once
	done     chan struct{}
}

func NewWorker(uc *DispatchNotification, interval time.Duration) *Worker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Worker{uc: uc, interval: interval, done: make(chan struct{})}
}

func (w *Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.stopOnce.Do(func() { close(w.done) })
			return
		case <-ticker.C:
			if _, err := w.uc.Tick(ctx, time.Now().UTC()); err != nil {
				log.Printf("[notifications/dispatch] tick err=%v", err)
			}
		}
	}
}

func (w *Worker) Done() <-chan struct{} { return w.done }
