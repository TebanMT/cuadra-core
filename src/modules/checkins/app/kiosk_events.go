package app

import (
	"sync"
	"time"
)

// KioskEventType enumerates the messages the loop publishes to the
// frontend. Two channels: synchronous SDK feedback (capture started, result)
// and asynchronous hardware events (connect/disconnect).
type KioskEventType string

const (
	EventReaderConnected    KioskEventType = "reader_connected"
	EventReaderDisconnected KioskEventType = "reader_disconnected"
	EventCaptureStarted     KioskEventType = "checkin_attempt_started"
	EventCheckinResult      KioskEventType = "checkin_result"
	EventCheckinError       KioskEventType = "checkin_error"
	EventKioskStarted       KioskEventType = "kiosk_started"
	EventKioskStopped       KioskEventType = "kiosk_stopped"
)

// KioskEvent is the wire shape the long-poll endpoint serializes.
type KioskEvent struct {
	Type      KioskEventType `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Checkin   *CheckinView   `json:"checkin,omitempty"`
	Message   string         `json:"message,omitempty"`
}

// KioskBroadcaster fans out KioskEvents to subscribers (the long-poll handler
// + tests). Subscribers register a buffered channel; if the channel is full
// the broadcaster drops the event for that subscriber rather than blocking
// the SDK loop.
//
// Lives outside the sidecar build tag so the cloud build can also publish
// checkin events (useful for tests / dashboards listening to push from a
// remote dev agent) without dragging the biometric SDK in.
type KioskBroadcaster struct {
	mu          sync.Mutex
	subscribers []chan KioskEvent
	bufSize     int
}

func NewKioskBroadcaster() *KioskBroadcaster {
	return &KioskBroadcaster{bufSize: 16}
}

// Subscribe returns a channel that receives all subsequent events + a cancel
// func that unsubscribes (and drains) the channel.
func (b *KioskBroadcaster) Subscribe() (<-chan KioskEvent, func()) {
	ch := make(chan KioskEvent, b.bufSize)
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
func (b *KioskBroadcaster) Publish(evt KioskEvent) {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	b.mu.Lock()
	subs := append([]chan KioskEvent{}, b.subscribers...)
	b.mu.Unlock()
	for _, sub := range subs {
		select {
		case sub <- evt:
		default:
			// Subscriber is slow/dead — drop. The next poll will resync via
			// the latest checkin row in the DB if needed.
		}
	}
}
