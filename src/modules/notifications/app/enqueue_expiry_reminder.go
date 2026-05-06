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
)

// EnqueueExpiryReminder is UC-038. The use case is a goroutine in cloud
// that wakes up periodically (default hourly), pulls memberships about to
// expire / just expired, and inserts notification_queue rows.
//
// Cadence (DA-38.1): 3 days before, day-of, 5 days after. Idempotency key
// `expiry_reminder:<member_id>:<offset>:<expiry_date>` ensures we never
// duplicate even if the loop runs twice per tick.
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

// Tick runs one pass of the scheduler. `today` is normally time.Now().UTC()
// — accepted as a parameter so tests can drive the clock. Returns the
// number of rows inserted.
func (uc *EnqueueExpiryReminder) Tick(ctx context.Context, today time.Time) (int, error) {
	today = truncateUTCDate(today)
	inserted := 0
	for _, stage := range defaultExpiryStages() {
		// Reminder fires `OffsetDays` ahead of expiry; 5d-post fires
		// 5 days AFTER expiry (so target_date = today - 5d).
		// Net: target = today - OffsetDays.
		target := today.AddDate(0, 0, -stage.OffsetDays)
		n, err := uc.tickStage(ctx, today, target, stage)
		if err != nil {
			return inserted, err
		}
		inserted += n
	}
	return inserted, nil
}

func (uc *EnqueueExpiryReminder) tickStage(ctx context.Context, today, target time.Time, stage expiryStage) (int, error) {
	inserted := 0
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		candidates, err := uc.Reader.FindExpiringOn(tx, target)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		for _, c := range candidates {
			if !c.GymWhatsAppReady {
				continue
			}
			if strings.TrimSpace(c.MemberPhone) == "" {
				continue
			}
			idempKey := fmt.Sprintf("expiry_reminder:%s:%d:%s",
				c.MemberID.String(), stage.OffsetDays, target.Format("2006-01-02"))
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
				today, today,
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

func truncateUTCDate(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
