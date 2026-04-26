//go:build sidecar

package app

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/shared/biometric"
)

// KioskLoop is the goroutine that drives the kiosko: it polls
// reader.Capture(), runs CheckinByFingerprint on each capture, and publishes
// events. It also wires reader.OnConnect / OnDisconnect to broadcast
// hardware state changes. Lives in app/ (not infrastructure) because it
// orchestrates a use case, not a stable adapter.
//
// Sidecar-only build tag: the loop directly depends on the biometric Reader
// surface; the cloud build never spins it up.
type KioskLoop struct {
	GymID       uuid.UUID
	Reader      biometric.Reader
	Fingerprint *CheckinByFingerprint
	Events      *KioskBroadcaster

	// CaptureBackoff is how long to wait between consecutive capture attempts
	// when the previous one returned ErrNoFingerDetected. Defaults to 200ms;
	// tests use 0 for fast spin.
	CaptureBackoff time.Duration

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
}

// NewKioskLoop wires the loop. Pass `nil` Reader to make the loop a no-op
// (useful when the kiosko is started but the lector is disabled at build).
func NewKioskLoop(gymID uuid.UUID, reader biometric.Reader, fp *CheckinByFingerprint, events *KioskBroadcaster) *KioskLoop {
	return &KioskLoop{
		GymID: gymID, Reader: reader, Fingerprint: fp, Events: events,
		CaptureBackoff: 200 * time.Millisecond,
	}
}

// Start spawns the loop goroutine. Idempotent — calling Start twice without
// Stop in between returns nil and leaves the existing goroutine alone.
func (l *KioskLoop) Start(parent context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return nil
	}
	if l.Reader == nil {
		l.Events.Publish(KioskEvent{Type: EventKioskStarted, Message: "lector deshabilitado"})
		l.running = true
		ctx, cancel := context.WithCancel(parent)
		l.cancel = cancel
		go func() {
			<-ctx.Done()
			l.Events.Publish(KioskEvent{Type: EventKioskStopped})
		}()
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	l.cancel = cancel
	l.running = true

	// Wire hardware-event callbacks. Note we register fresh callbacks each
	// Start; the Reader implementation is responsible for keeping them tied
	// to its lifecycle (mocks just append).
	l.Reader.OnConnect(func() {
		l.Events.Publish(KioskEvent{Type: EventReaderConnected})
	})
	l.Reader.OnDisconnect(func() {
		l.Events.Publish(KioskEvent{Type: EventReaderDisconnected})
	})

	l.Events.Publish(KioskEvent{Type: EventKioskStarted})
	go l.run(ctx)
	return nil
}

// Stop signals the goroutine to exit. Safe to call multiple times.
func (l *KioskLoop) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running {
		return
	}
	if l.cancel != nil {
		l.cancel()
	}
	l.running = false
}

// IsRunning reports whether the loop is currently active.
func (l *KioskLoop) IsRunning() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

// run is the actual goroutine body. We keep it in a method so tests that
// want a single tick (without spinning) can call it directly with a context
// that fires after one capture.
func (l *KioskLoop) run(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			l.Events.Publish(KioskEvent{Type: EventKioskStopped})
			return
		}
		capture, err := l.Reader.Capture(ctx)
		if err != nil {
			if errors.Is(err, biometric.ErrNoFingerDetected) {
				l.sleep(ctx, l.CaptureBackoff)
				continue
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				l.Events.Publish(KioskEvent{Type: EventKioskStopped})
				return
			}
			if errors.Is(err, biometric.ErrNotAvailable) {
				// Hardware just dropped — onDisconnect should also fire, but
				// publish defensively in case the SDK didn't.
				l.Events.Publish(KioskEvent{Type: EventReaderDisconnected})
				l.sleep(ctx, time.Second)
				continue
			}
			log.Printf("[kiosk] capture error: %v", err)
			l.Events.Publish(KioskEvent{Type: EventCheckinError, Message: err.Error()})
			l.sleep(ctx, l.CaptureBackoff)
			continue
		}

		l.Events.Publish(KioskEvent{Type: EventCaptureStarted})
		view, err := l.Fingerprint.Execute(ctx, CheckinByFingerprintInput{
			GymID:   l.GymID,
			Capture: capture,
		})
		if err != nil {
			l.Events.Publish(KioskEvent{Type: EventCheckinError, Message: err.Error()})
			continue
		}
		l.Events.Publish(KioskEvent{Type: EventCheckinResult, Checkin: view})
	}
}

// sleep is a context-aware wait. Returns early if ctx fires.
func (l *KioskLoop) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
