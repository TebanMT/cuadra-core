package app

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ExpiredTrial is a gym still on trial whose trial_ends_at already passed.
type ExpiredTrial struct {
	GymID       uuid.UUID
	TrialEndsAt time.Time
}

// ExpiredTrialReader finds the trials to close: plan trial, status active and
// trial_ends_at <= now. A trial already cancelled, extended or paid is not
// returned, so the job never touches it twice.
type ExpiredTrialReader interface {
	FindExpiredTrials(tx sharedDomain.Transaction, now time.Time) ([]ExpiredTrial, error)
}

// ExpireTrials is the cloud job that closes the trials that ended without a
// payment (UC-002 "Trial vencido"). Without it a trial never ended: the
// sidecar only restricts what the cloud confirms through sync.
//
// It records an EventCancelled per gym; RecordEvent applies Gym.EndTrial
// (status cancelled + TrialGraceDays of full access in subscription_ends_at),
// writes the subscription_events history and touches the gym in the sync
// journal so the sidecars pull it. After the grace the sidecar runs the
// reception read-only, with check-in still open.
//
// external_id includes trial_ends_at: replays of the same trial are no-ops,
// and a trial extended later (ExtendTrial) gets its own close when it ends.
type ExpireTrials struct {
	Reader   ExpiredTrialReader
	Recorder EventRecorder
	UoW      sharedDomain.UnitOfWork
}

func NewExpireTrials(reader ExpiredTrialReader, recorder EventRecorder, uow sharedDomain.UnitOfWork) *ExpireTrials {
	return &ExpireTrials{Reader: reader, Recorder: recorder, UoW: uow}
}

// Tick closes every expired trial and returns how many it applied. A gym
// that fails is logged and retried on the next tick.
func (uc *ExpireTrials) Tick(ctx context.Context, now time.Time) (int, error) {
	if handled, n, err := sharedDomain.TenantWorker(ctx, uc.UoW, func(scoped context.Context) (int, error) { return uc.Tick(scoped, now) }); handled {
		return n, err
	}
	tx, err := uc.UoW.Query(ctx)
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		return 0, sharedDomain.NewUnexpectedError(err)
	}
	trials, err := uc.Reader.FindExpiredTrials(tx, now)
	sharedDomain.CloseQuery(tx)
	if err != nil {
		return 0, sharedDomain.NewUnexpectedError(err)
	}
	closed := 0
	for _, trial := range trials {
		out, err := uc.Recorder.Execute(ctx, RecordEventInput{
			GymID:      trial.GymID,
			Provider:   subDomain.ProviderManual,
			Type:       subDomain.EventCancelled,
			ExternalID: TrialExpiredExternalID(trial),
			OccurredAt: now,
			RawPayload: map[string]any{
				"reason":        "trial_ended_without_payment",
				"trial_ends_at": trial.TrialEndsAt.UTC().Format(time.RFC3339),
			},
		})
		if err != nil {
			log.Printf("[subscriptions/trial_expiry] skip gym=%s err=%v", trial.GymID, err)
			continue
		}
		if out.Applied {
			closed++
		}
	}
	return closed, nil
}

// TrialExpiredExternalID is the idempotency key of a trial close.
func TrialExpiredExternalID(trial ExpiredTrial) string {
	return fmt.Sprintf("trial-expired:%s:%s", trial.GymID, trial.TrialEndsAt.UTC().Format(time.RFC3339))
}

// TrialExpiryWorker runs ExpireTrials on a ticker (cmd/server). It ticks once
// at start so a restart never waits a full interval.
type TrialExpiryWorker struct {
	uc       *ExpireTrials
	interval time.Duration
	now      func() time.Time

	stopOnce sync.Once
	done     chan struct{}
}

func NewTrialExpiryWorker(uc *ExpireTrials, interval time.Duration) *TrialExpiryWorker {
	if interval <= 0 {
		interval = time.Hour
	}
	return &TrialExpiryWorker{
		uc:       uc,
		interval: interval,
		now:      func() time.Time { return time.Now().UTC() },
		done:     make(chan struct{}),
	}
}

func (w *TrialExpiryWorker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	// WithoutCancel: shutdown stops the loop, never a tick in flight.
	w.runOnce(context.WithoutCancel(ctx))
	for {
		select {
		case <-ctx.Done():
			w.stopOnce.Do(func() { close(w.done) })
			return
		case <-ticker.C:
			w.runOnce(context.WithoutCancel(ctx))
		}
	}
}

func (w *TrialExpiryWorker) runOnce(ctx context.Context) {
	n, err := w.uc.Tick(ctx, w.now())
	if err != nil {
		log.Printf("[subscriptions/trial_expiry] tick err=%v", err)
		return
	}
	if n > 0 {
		log.Printf("[subscriptions/trial_expiry] closed=%d", n)
	}
}

func (w *TrialExpiryWorker) Done() <-chan struct{} { return w.done }
