package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	usersRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// reminderStage describes one cron-fired notification stage. Order is the
// canonical 30 → 14 → 3 → 0 ramp the founder approved; idempotency is per
// (gym, days_before, ends_at) so each stage fires at most once per cycle.
type reminderStage struct {
	DaysBefore  int
	TemplateKey string
}

func renewalStages() []reminderStage {
	return []reminderStage{
		{DaysBefore: 30, TemplateKey: "oxxo_renewal_reminder_30d"},
		{DaysBefore: 14, TemplateKey: "oxxo_renewal_reminder_14d"},
		{DaysBefore: 3, TemplateKey: "oxxo_renewal_reminder_3d"},
		{DaysBefore: 0, TemplateKey: "oxxo_renewal_reminder_today"},
	}
}

// CheckoutStarter is the slice of *StartCheckout the renewal cron needs.
// Defined here so unit tests can swap a fake — *StartCheckout satisfies it
// directly without changes to its signature.
type CheckoutStarter interface {
	Execute(ctx context.Context, in StartCheckoutInput) (StartCheckoutOutput, error)
}

// EventRecorder is the slice of *RecordEvent the cancel cron needs. Same
// pattern as CheckoutStarter — *RecordEvent satisfies it as-is.
type EventRecorder interface {
	Execute(ctx context.Context, in RecordEventInput) (RecordEventOutput, error)
}

// RunOXXORenewalReminders es el cron del cloud que mantiene viva la
// renovación anual de gyms en OXXO. Cada tick (diario) genera un link de
// re-checkout y encola una notificación por gym candidato en cada una de
// las ventanas {30d, 14d, 3d, día 0}. Idempotencia vía notification_queue
// .idempotency_key — correr 2 veces el mismo día no duplica.
//
// Tarjeta-anual NO entra: la Stripe Subscription renueva sola y mete
// invoice.paid → EventRenewed → SubscriptionEndsAt extendido automáticamente.
type RunOXXORenewalReminders struct {
	Reader        OXXORenewalReader
	Notifications notiRepo.NotificationRepository
	Gyms          gymRepo.GymRepository
	Users         usersRepo.UserRepository
	Checkout      CheckoutStarter
	UoW           sharedDomain.UnitOfWork
	NowFunc       func() time.Time
}

func NewRunOXXORenewalReminders(
	reader OXXORenewalReader,
	notifications notiRepo.NotificationRepository,
	gyms gymRepo.GymRepository,
	users usersRepo.UserRepository,
	checkout CheckoutStarter,
	uow sharedDomain.UnitOfWork,
) *RunOXXORenewalReminders {
	return &RunOXXORenewalReminders{
		Reader:        reader,
		Notifications: notifications,
		Gyms:          gyms,
		Users:         users,
		Checkout:      checkout,
		UoW:           uow,
		NowFunc:       func() time.Time { return time.Now().UTC() },
	}
}

// Tick runs one pass over all four stages. Returns the count of
// notifications enqueued (a number > 0 implies progress; 0 is the steady
// state). Errors at the reader / DB level surface; per-gym failures log
// and continue so one bad gym doesn't stall the whole batch.
func (uc *RunOXXORenewalReminders) Tick(ctx context.Context, now time.Time) (int, error) {
	enqueued := 0
	for _, stage := range renewalStages() {
		n, err := uc.tickStage(ctx, now, stage)
		if err != nil {
			return enqueued, err
		}
		enqueued += n
	}
	return enqueued, nil
}

func (uc *RunOXXORenewalReminders) tickStage(ctx context.Context, now time.Time, stage reminderStage) (int, error) {
	target := now.Add(time.Duration(stage.DaysBefore) * 24 * time.Hour)
	windowStart := target.Add(-12 * time.Hour)
	windowEnd := target.Add(12 * time.Hour)

	var candidates []OXXORenewalCandidate
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		cs, err := uc.Reader.FindRenewalCandidates(tx, windowStart, windowEnd)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		candidates = cs
		return nil
	})
	if err != nil {
		return 0, err
	}

	enqueued := 0
	for _, c := range candidates {
		ok, err := uc.enqueueOne(ctx, c, stage, now)
		if err != nil {
			log.Printf("[subscriptions/oxxo_reminder] skip gym=%s stage=%dd err=%v", c.GymID, stage.DaysBefore, err)
			continue
		}
		if ok {
			enqueued++
		}
	}
	return enqueued, nil
}

// enqueueOne genera el link de re-checkout y planta una notification_queue
// row por (gym, days_before, ends_at). Devuelve (true, nil) cuando insertó
// un row nuevo; (false, nil) cuando ya existía la fila idempotente.
func (uc *RunOXXORenewalReminders) enqueueOne(ctx context.Context, c OXXORenewalCandidate, stage reminderStage, now time.Time) (bool, error) {
	endsAtKey := c.SubscriptionEndsAt.UTC().Format("2006-01-02")
	idempKey := fmt.Sprintf("oxxo_renewal_reminder:%s:%d:%s", c.GymID, stage.DaysBefore, endsAtKey)

	var (
		ownerUserID uuid.UUID
		ownerEmail  string
		ownerPhone  string
		gymName     = c.GymName
		alreadyEnq  bool
	)

	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		existing, err := uc.Notifications.GetByIdempotencyKey(tx, c.GymID, idempKey)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if existing != nil {
			alreadyEnq = true
			return nil
		}
		users, err := uc.Users.ListByGym(tx, c.GymID)
		if err != nil {
			return err
		}
		owner := pickOwner(users)
		if owner == nil {
			return errors.New("no active owner for gym")
		}
		ownerUserID = owner.ID
		ownerEmail = owner.Email
		if owner.Phone != nil {
			ownerPhone = strings.TrimSpace(*owner.Phone)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if alreadyEnq {
		return false, nil
	}

	// El use case StartCheckout maneja su propia tx (Query). Lo llamamos
	// FUERA del Command anterior porque mezclar txs en GORM nested rompe
	// el lease. Si la red a Stripe falla, devolvemos error y el next tick
	// reintenta — la idempotency key todavía no existe (no creamos la
	// notificación), así que el reintento es seguro.
	checkoutOut, err := uc.Checkout.Execute(ctx, StartCheckoutInput{
		GymID:         c.GymID,
		UserID:        ownerUserID,
		Provider:      subDomain.ProviderStripe,
		Plan:          gymDomain.PlanStandardAnnual,
		PaymentMethod: "oxxo",
	})
	if err != nil {
		return false, fmt.Errorf("start checkout: %w", err)
	}

	channel, recipientAddress := pickChannel(c.GymWhatsAppReady, ownerPhone, ownerEmail)
	if recipientAddress == "" {
		return false, errors.New("no recipient address (owner sin phone ni email)")
	}

	payload := map[string]string{
		"gym_name":    gymName,
		"voucher_url": checkoutOut.URL,
		"expires_on":  c.SubscriptionEndsAt.UTC().Format("02 ene 2006"),
	}
	// fallback_email destrabado el path WhatsApp→email del dispatcher
	// (dispatch_notification.go tryEmailFallback) para gyms que aún no
	// conectaron WhatsApp. Si ya elegimos email como canal primario, no
	// tiene efecto.
	if ownerEmail != "" && channel == notiDomain.ChannelWhatsApp {
		payload["fallback_email"] = ownerEmail
	}

	return true, uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		// Re-check idempotency adentro del Command para cubrir la race entre
		// dos workers (improbable hoy — un solo cron — pero el lock cuesta cero).
		existing, err := uc.Notifications.GetByIdempotencyKey(tx, c.GymID, idempKey)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if existing != nil {
			return nil
		}
		n, err := notiDomain.New(
			uuid.New(), c.GymID, ownerUserID,
			channel, stage.TemplateKey,
			notiDomain.RecipientUser,
			recipientAddress,
			payload,
			now, now,
			&idempKey,
		)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if _, err := uc.Notifications.Create(tx, n); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		return nil
	})
}

// pickOwner returns the first active owner user from `users`, or nil.
func pickOwner(users []*userDomain.User) *userDomain.User {
	for _, u := range users {
		if u == nil {
			continue
		}
		if u.Role == "owner" && u.Active {
			return u
		}
	}
	return nil
}

// pickChannel decide canal + destinatario. WhatsApp gana si el gym tiene
// la integración conectada Y el dueño tiene phone registrado. Si falla
// cualquiera de los dos, caemos a email (mejor que silenciar). Si el
// dueño no tiene ni phone ni email registrado, recipientAddress="" y el
// caller falla la fila — no debería pasar (el signup pide email).
func pickChannel(gymWhatsAppReady bool, ownerPhone, ownerEmail string) (channel, recipient string) {
	if gymWhatsAppReady && ownerPhone != "" {
		return notiDomain.ChannelWhatsApp, ownerPhone
	}
	if ownerEmail != "" {
		return notiDomain.ChannelEmail, ownerEmail
	}
	return notiDomain.ChannelWhatsApp, ""
}

// CancelExpiredOXXO es el job que, tras 7 días de gracia post-vencimiento
// sin pago, planta un EventCancelled para el gym. El RecordEvent use case
// es quien aplica la transición a SubscriptionStatus=cancelled — este job
// sólo decide CUÁNDO. external_id determinístico → idempotente entre runs.
//
// La gracia (7d) está alineada con el principio "honor system" del founder:
// no bloqueamos hard apenas vence, damos margen al cliente que se atrasó.
// El sidecar, además, sólo bloquea cuando IsAccessHardBlocked devuelve true
// (cancelled + SubscriptionEndsAt vencido) — el gym sigue operando offline
// hasta que el sync confirma el cancel.
type CancelExpiredOXXO struct {
	Reader   OXXORenewalReader
	Recorder EventRecorder
	UoW      sharedDomain.UnitOfWork
	NowFunc  func() time.Time
	Grace    time.Duration
}

func NewCancelExpiredOXXO(
	reader OXXORenewalReader,
	recorder EventRecorder,
	uow sharedDomain.UnitOfWork,
) *CancelExpiredOXXO {
	return &CancelExpiredOXXO{
		Reader:   reader,
		Recorder: recorder,
		UoW:      uow,
		NowFunc:  func() time.Time { return time.Now().UTC() },
		Grace:    7 * 24 * time.Hour,
	}
}

// Tick busca gyms anuales OXXO con SubscriptionEndsAt + grace en el pasado
// y registra EventCancelled. Devuelve el número aplicado.
func (uc *CancelExpiredOXXO) Tick(ctx context.Context, now time.Time) (int, error) {
	cutoff := now.Add(-uc.Grace)
	var candidates []OXXORenewalCandidate
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		cs, err := uc.Reader.FindExpiredOXXO(tx, cutoff)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		candidates = cs
		return nil
	})
	if err != nil {
		return 0, err
	}
	cancelled := 0
	for _, c := range candidates {
		endsAtKey := c.SubscriptionEndsAt.UTC().Format("2006-01-02")
		externalID := fmt.Sprintf("oxxo-expiry-cancel:%s:%s", c.GymID, endsAtKey)
		out, err := uc.Recorder.Execute(ctx, RecordEventInput{
			GymID:      c.GymID,
			Provider:   subDomain.ProviderManual,
			Type:       subDomain.EventCancelled,
			ExternalID: externalID,
			OccurredAt: now,
			RawPayload: map[string]any{
				"reason":  "oxxo_annual_expired_grace_exceeded",
				"ends_at": c.SubscriptionEndsAt.UTC().Format(time.RFC3339),
			},
		})
		if err != nil {
			log.Printf("[subscriptions/oxxo_cancel] skip gym=%s err=%v", c.GymID, err)
			continue
		}
		if out.Applied {
			cancelled++
		}
	}
	return cancelled, nil
}

// OXXORenewalWorker arranca como goroutine al lado de los workers de
// notifications en cmd/server/main.go. Corre reminder + cancel en cada
// tick — son baratos y comparten el mismo reader.
type OXXORenewalWorker struct {
	reminder *RunOXXORenewalReminders
	cancel   *CancelExpiredOXXO
	interval time.Duration
	now      func() time.Time

	stopOnce sync.Once
	done     chan struct{}
}

func NewOXXORenewalWorker(reminder *RunOXXORenewalReminders, cancel *CancelExpiredOXXO, interval time.Duration) *OXXORenewalWorker {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &OXXORenewalWorker{
		reminder: reminder,
		cancel:   cancel,
		interval: interval,
		now:      func() time.Time { return time.Now().UTC() },
		done:     make(chan struct{}),
	}
}

func (w *OXXORenewalWorker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.runOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			w.stopOnce.Do(func() { close(w.done) })
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func (w *OXXORenewalWorker) runOnce(ctx context.Context) {
	now := w.now()
	if w.reminder != nil {
		if n, err := w.reminder.Tick(ctx, now); err != nil {
			log.Printf("[subscriptions/oxxo_reminder] tick err=%v", err)
		} else if n > 0 {
			log.Printf("[subscriptions/oxxo_reminder] enqueued=%d", n)
		}
	}
	if w.cancel != nil {
		if n, err := w.cancel.Tick(ctx, now); err != nil {
			log.Printf("[subscriptions/oxxo_cancel] tick err=%v", err)
		} else if n > 0 {
			log.Printf("[subscriptions/oxxo_cancel] cancelled=%d", n)
		}
	}
}

func (w *OXXORenewalWorker) Done() <-chan struct{} { return w.done }
