package contact_attempt

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNew_Defaults(t *testing.T) {
	now := time.Now().UTC()
	a, err := New(uuid.New(), uuid.New(), uuid.New(), uuid.New(), now, nil, nil, now)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if a.Channel != nil || a.Note != nil {
		t.Errorf("expected nil channel/note, got %+v / %+v", a.Channel, a.Note)
	}
	if a.Version != 1 {
		t.Errorf("version = %d, want 1", a.Version)
	}
}

func TestNew_RejectsInvalidChannel(t *testing.T) {
	now := time.Now().UTC()
	bad := "carrier_pigeon"
	_, err := New(uuid.New(), uuid.New(), uuid.New(), uuid.New(), now, &bad, nil, now)
	if err == nil {
		t.Fatal("expected error for invalid channel")
	}
}

func TestNew_AcceptsKnownChannels(t *testing.T) {
	now := time.Now().UTC()
	for _, ch := range []string{ChannelWhatsApp, ChannelPhone, ChannelInPerson, ChannelOther} {
		c := ch
		if _, err := New(uuid.New(), uuid.New(), uuid.New(), uuid.New(), now, &c, nil, now); err != nil {
			t.Errorf("channel %q: %v", c, err)
		}
	}
}

func TestNew_RejectsTooLongNote(t *testing.T) {
	now := time.Now().UTC()
	note := strings.Repeat("x", 1001)
	_, err := New(uuid.New(), uuid.New(), uuid.New(), uuid.New(), now, nil, &note, now)
	if err == nil {
		t.Fatal("expected error for long note")
	}
}

func TestNew_TrimsAndNullifiesEmptyNote(t *testing.T) {
	now := time.Now().UTC()
	note := "   "
	a, err := New(uuid.New(), uuid.New(), uuid.New(), uuid.New(), now, nil, &note, now)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if a.Note != nil {
		t.Errorf("expected nil note for whitespace input, got %q", *a.Note)
	}
}
