package app

import (
	n "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	"testing"
	"time"
)

func TestHeldContentFailsClosed(t *testing.T) {
	for _, key := range []string{"operator_welcome_pin", "owner_welcome", "whatsapp_connect_otp", "oxxo_renewal_reminder_today", "expiry_reminder_today", "future_template"} {
		if recoveryContentBlock(&n.Notification{TemplateKey: key, Channel: n.ChannelWhatsApp}) == "" {
			t.Fatal("unsafe", key)
		}
	}
	for _, payload := range []map[string]string{{"message": "https://example.test/expired"}, {"token": "expired"}, {"pin": "1234"}} {
		if recoveryContentBlock(&n.Notification{TemplateKey: "broadcast_freeform", Channel: n.ChannelWhatsApp, Payload: payload}) == "" {
			t.Fatal("unsafe payload")
		}
	}
	msg := &n.Notification{TemplateKey: "broadcast_freeform", Channel: n.ChannelWhatsApp, Status: n.StatusHeld, CreatedAt: time.Now().Add(-30 * 24 * time.Hour), Version: 3}
	original := msg.CreatedAt
	now := time.Now()
	msg.ResolveHeld(3, "send", now)
	if msg.Status != n.StatusPending || msg.RecoveryAt == nil || isStale(msg.TemplateKey, *msg.RecoveryAt, now) || !msg.CreatedAt.Equal(original) {
		t.Fatal("invalid recovery freshness")
	}
}
