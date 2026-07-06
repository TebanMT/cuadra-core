package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// EnqueueExpiryReminder is UC-038. The use case is a goroutine in cloud
// that wakes up periodically (default hourly), pulls memberships about to
// expire / just expired, and inserts notification_queue rows.
//
// Cadence (DA-38.1): 3 days before, day-of, 5 days after — evaluados
// contra el día calendario LOCAL de cada gym (el reader hace la cuenta
// por tz). Idempotency key `expiry_reminder:<member_id>:<offset>:<expiry_date>`
// ensures we never duplicate even if the loop runs twice per tick.
//
// Horario de envío: el tick corre 24/7 (barato e idempotente; gyms en
// husos distintos cruzan su medianoche a horas distintas), pero cada fila
// se agenda con scheduled_for dentro de la ventana 8AM–9PM LOCAL del gym
// — el dispatcher ya respeta scheduled_for, así que ningún socio recibe
// un recordatorio de madrugada. Gatear el TICK por horario (alternativa
// considerada) rompería con multi-tz y con el catch-up tras caídas.
type EnqueueExpiryReminder struct {
	Notifications notiRepo.NotificationRepository
	Reader        ExpiryReader
	UoW           sharedDomain.UnitOfWork
}

func NewEnqueueExpiryReminder(
	notifications notiRepo.NotificationRepository,
	reader ExpiryReader,
	uow sharedDomain.UnitOfWork,
) *EnqueueExpiryReminder {
	return &EnqueueExpiryReminder{Notifications: notifications, Reader: reader, UoW: uow}
}

// expiryStage maps a day-offset to (templateKey, idempotency suffix).
type expiryStage struct {
	OffsetDays  int
	TemplateKey string
}

func defaultExpiryStages() []expiryStage {
	return []expiryStage{
		{OffsetDays: -3, TemplateKey: "expiry_reminder_3d"},
		{OffsetDays: 0, TemplateKey: "expiry_reminder_today"},
		{OffsetDays: +5, TemplateKey: "expiry_reminder_5d_post"},
	}
}

// Tick runs one pass of the scheduler. `now` is normally time.Now().UTC()
// — accepted as a parameter so tests can drive the clock. Returns the
// number of rows inserted.
func (uc *EnqueueExpiryReminder) Tick(ctx context.Context, now time.Time) (int, error) {
	inserted := 0
	for _, stage := range defaultExpiryStages() {
		n, err := uc.tickStage(ctx, now, stage)
		if err != nil {
			return inserted, err
		}
		inserted += n
	}
	return inserted, nil
}

func (uc *EnqueueExpiryReminder) tickStage(ctx context.Context, now time.Time, stage expiryStage) (int, error) {
	inserted := 0
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		candidates, err := uc.Reader.FindDueForStage(tx, now, stage.OffsetDays)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		for _, c := range candidates {
			// ADR-009: los recordatorios salen por el MASTER SENDER de Tinta,
			// NO por el número propio del gym. No gatear por número conectado
			// del gym (eso es Plus / UC-037): un gym Standard nunca conecta el
			// suyo, así que gatear aquí mataba el feature en Standard. El
			// dispatcher resuelve el `from` (master vs propio); aquí sólo
			// exigimos que el socio tenga teléfono. Mismo criterio que
			// enqueue_receipt / enqueue_owner_alert.
			if strings.TrimSpace(c.MemberPhone) == "" {
				continue
			}
			// La fecha de la llave es el expiry que matcheó la etapa —
			// idéntico al `target` del esquema anterior (target = hoy -
			// offset = expiry matcheado), así que los recordatorios ya
			// enviados con el formato viejo no se duplican.
			idempKey := fmt.Sprintf("expiry_reminder:%s:%d:%s",
				c.MemberID.String(), stage.OffsetDays, c.ExpiryDate.Format("2006-01-02"))
			existing, err := uc.Notifications.GetByIdempotencyKey(tx, c.GymID, idempKey)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if existing != nil {
				continue
			}
			vars := map[string]string{
				"member_first_name": firstName(c.MemberFullName),
				"gym_name":          c.GymName,
				"expiry_date":       c.ExpiryDate.Format("02 ene 2006"),
			}
			n, err := notiDomain.New(
				uuid.New(), c.GymID, c.MemberID,
				notiDomain.ChannelWhatsApp,
				stage.TemplateKey,
				notiDomain.RecipientMember,
				c.MemberPhone,
				vars,
				scheduleWithinSendWindow(c.GymTimezone, now), now,
				&idempKey,
			)
			if err != nil {
				log.Printf("[notifications] expiry_reminder skip member=%s err=%v", c.MemberID, err)
				continue
			}
			if _, err := uc.Notifications.Create(tx, n); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			inserted++
		}
		return nil
	})
	return inserted, err
}

// Ventana de envío de recordatorios, en hora LOCAL del gym. Aplica sólo a
// mensajes masivos no-transaccionales (estos); los recibos post-cobro
// siguen saliendo al momento — el socio acaba de pagar y lo espera.
const (
	sendWindowStartHour = 8  // 8:00 AM
	sendWindowEndHour   = 21 // 9:00 PM (exclusivo: a las 21:00 ya no se envía)
)

// scheduleWithinSendWindow agenda el envío dentro de la ventana 8AM–9PM
// local del gym: antes de las 8 → hoy 8:00; dentro de la ventana → ahora
// mismo; a partir de las 21:00 → mañana 8:00. El dispatcher respeta
// scheduled_for, así que esto basta — no hace falta gatear el tick.
func scheduleWithinSendWindow(tzName string, now time.Time) time.Time {
	loc := tz.LocationOrUTC(tzName)
	local := now.In(loc)
	switch {
	case local.Hour() < sendWindowStartHour:
		return time.Date(local.Year(), local.Month(), local.Day(),
			sendWindowStartHour, 0, 0, 0, loc).UTC()
	case local.Hour() >= sendWindowEndHour:
		next := local.AddDate(0, 0, 1)
		return time.Date(next.Year(), next.Month(), next.Day(),
			sendWindowStartHour, 0, 0, 0, loc).UTC()
	default:
		return now
	}
}

// Scheduler runs Tick on a ticker until ctx is cancelled. Cloud main.go
// launches it in a goroutine. Errors are logged, never propagated — the
// scheduler keeps running.
type Scheduler struct {
	uc       *EnqueueExpiryReminder
	interval time.Duration
	now      func() time.Time

	stopOnce sync.Once
	done     chan struct{}
}

func NewScheduler(uc *EnqueueExpiryReminder, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Scheduler{
		uc:       uc,
		interval: interval,
		now:      func() time.Time { return time.Now().UTC() },
		done:     make(chan struct{}),
	}
}

// Start blocks until ctx is cancelled. Run it from a goroutine.
func (s *Scheduler) Start(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	// Fire once at startup so dev iterations don't have to wait an hour.
	if n, err := s.uc.Tick(ctx, s.now()); err != nil {
		log.Printf("[notifications/scheduler] startup tick err=%v", err)
	} else if n > 0 {
		log.Printf("[notifications/scheduler] startup tick enqueued=%d", n)
	}
	for {
		select {
		case <-ctx.Done():
			s.stopOnce.Do(func() { close(s.done) })
			return
		case <-ticker.C:
			n, err := s.uc.Tick(ctx, s.now())
			if err != nil {
				log.Printf("[notifications/scheduler] tick err=%v", err)
				continue
			}
			if n > 0 {
				log.Printf("[notifications/scheduler] tick enqueued=%d", n)
			}
		}
	}
}

// Done returns a channel closed once Start has exited.
func (s *Scheduler) Done() <-chan struct{} { return s.done }
