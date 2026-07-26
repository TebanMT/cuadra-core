package app

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// BioEventType enumerates what the sidecar pushes to the FE over
// GET /api/v1/biometric/events (SSE). Three families:
//
//   - hardware: reader_connected / reader_disconnected
//   - check-in: attempt → result | no_match | error, plus sample_rejected
//     for "vuelve a apoyar el dedo" feedback
//   - enroll: started → progress×N → completed | failed (failed carries a
//     Code: timeout, cancelled, fingerprint_collision, enrollment_invalid,
//     engine_error, internal)
type BioEventType string

const (
	BioReaderConnected    BioEventType = "reader_connected"
	BioReaderDisconnected BioEventType = "reader_disconnected"

	BioSampleRejected BioEventType = "sample_rejected"

	BioCheckinAttempt BioEventType = "checkin_attempt_started"
	BioCheckinResult  BioEventType = "checkin_result"
	BioCheckinNoMatch BioEventType = "checkin_no_match"
	BioCheckinError   BioEventType = "checkin_error"

	BioEnrollStarted   BioEventType = "enroll_started"
	BioEnrollProgress  BioEventType = "enroll_progress"
	BioEnrollCompleted BioEventType = "enroll_completed"
	BioEnrollFailed    BioEventType = "enroll_failed"
)

// Enroll failure codes (BioEvent.Code on enroll_failed).
const (
	EnrollFailTimeout    = "timeout"
	EnrollFailCancelled  = "cancelled"
	EnrollFailCollision  = "fingerprint_collision"
	EnrollFailInvalidSet = "enrollment_invalid"
	EnrollFailEngine     = "engine_error"
	EnrollFailInternal   = "internal"
)

// BioEnrollState is the enroll-session payload attached to enroll_* events.
type BioEnrollState struct {
	SessionID uuid.UUID
	MemberID  uuid.UUID
	Captured  int
	Required  int
	ExpiresAt time.Time
	// FingerprintIDs se llena sólo en enroll_completed.
	FingerprintIDs []uuid.UUID
	// Data carries the fingerprint_collision contract fields
	// (existing_member_id, existing_member_name) on enroll_failed.
	Data map[string]any
}

// BioEvent is what the hub publishes and the SSE controller serializes.
type BioEvent struct {
	Type      BioEventType
	Timestamp time.Time
	// Message is operator-facing Spanish (errors / no-match).
	Message string
	// Code disambiguates machine-readable variants (sample_rejected codes
	// del helper, enroll failure codes de arriba).
	Code string
	// Quality is the helper's categorical sample quality (DP_QUALITY_*).
	Quality string
	// ReaderName / ReaderSerial acompañan los eventos reader_*.
	ReaderName   string
	ReaderSerial string
	// Checkin viaja en checkin_result — mismo CheckinView que los endpoints.
	Checkin *CheckinView
	// Enroll viaja en los eventos enroll_*.
	Enroll *BioEnrollState
}

// BioBroadcaster fans out BioEvents to subscribers (the SSE handler +
// tests). Subscribers register a buffered channel; if the channel is full
// the broadcaster drops the event for that subscriber rather than blocking
// the engine's dispatch goroutine.
type BioBroadcaster struct {
	mu          sync.Mutex
	subscribers []chan BioEvent
	bufSize     int
}

func NewBioBroadcaster() *BioBroadcaster {
	return &BioBroadcaster{bufSize: 32}
}

// Subscribe returns a channel that receives all subsequent events + a cancel
// func that unsubscribes (and closes) the channel.
func (b *BioBroadcaster) Subscribe() (<-chan BioEvent, func()) {
	ch := make(chan BioEvent, b.bufSize)
	b.mu.Lock()
	b.subscribers = append(b.subscribers, ch)
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, sub := range b.subscribers {
			if sub == ch {
				b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
				close(ch)
				return
			}
		}
	}
	return ch, cancel
}

// Publish broadcasts an event to every active subscriber (non-blocking; full
// buffers drop the event for that subscriber).
func (b *BioBroadcaster) Publish(evt BioEvent) {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	b.mu.Lock()
	subs := append([]chan BioEvent{}, b.subscribers...)
	b.mu.Unlock()
	for _, sub := range subs {
		select {
		case sub <- evt:
		default:
			// Subscriber lento/muerto — se descarta. El FE se resincroniza
			// con /biometric/status y la lista de checkins recientes.
		}
	}
}
