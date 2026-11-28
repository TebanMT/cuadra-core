package app

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
)

func TestRenderHistoryBody_ReceiptKeepsTextWithoutURL(t *testing.T) {
	got := renderHistoryBody("receipt_membership", map[string]string{
		"member_first_name": "Ana",
		"amount":            "450.00",
		"membership_type":   "Mensual",
		"expiry_date":       "27 dic 2026",
		"gym_name":          "Gym Centro",
	})
	want := "Hola Ana. Recibimos tu pago de $450.00 por Mensual en Gym Centro. Vigencia hasta el 27 dic 2026. Comprobante: [PDF del comprobante adjunto]"
	if got != want {
		t.Fatalf("body =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderHistoryBody_MissingVarNeverHidesMessage(t *testing.T) {
	got := renderHistoryBody("expiry_reminder_3d", map[string]string{"member_first_name": "Luis"})
	if !strings.Contains(got, "Hola Luis") || !strings.Contains(got, "—") {
		t.Fatalf("variable ausente debe quedar como marcador, got %q", got)
	}
	if strings.Contains(got, "{") {
		t.Fatalf("no deben quedar placeholders crudos: %q", got)
	}
}

func TestRenderHistoryBody_UnknownTemplateFallsBackToMessage(t *testing.T) {
	if got := renderHistoryBody("future_key", map[string]string{"message": " hola "}); got != "hola" {
		t.Fatalf("got %q", got)
	}
}

func TestMemberNotificationView_StatesAndReasons(t *testing.T) {
	now := time.Unix(5000, 0).UTC()
	base := func(status string) *notiDomain.Notification {
		return &notiDomain.Notification{
			ID: uuid.New(), GymID: uuid.New(), RecipientID: uuid.New(),
			Channel: notiDomain.ChannelWhatsApp, TemplateKey: "expiry_reminder_today",
			RecipientType: notiDomain.RecipientMember, RecipientAddress: "+525511112222",
			Payload: map[string]string{"member_first_name": "Eva", "gym_name": "Gym"},
			Status:  status, Version: 1, CreatedAt: now, ScheduledFor: now,
		}
	}

	delivered := base(notiDomain.StatusSent)
	delivered.DeliveredAt = &now
	v := memberNotificationView(delivered)
	if v.DeliveryState != notiDomain.DeliveryDelivered || v.DeliveredAt == nil || v.FailureReason != "" {
		t.Fatalf("delivered view = %+v", v)
	}
	if v.Kind != "reminder" || v.Title == "" {
		t.Fatalf("kind/title = %q/%q", v.Kind, v.Title)
	}

	// Una fila re-armada (pending) con acuses viejos no los expone.
	rearmed := base(notiDomain.StatusPending)
	rearmed.DeliveredAt = &now
	if v := memberNotificationView(rearmed); v.DeliveredAt != nil || v.DeliveryState != notiDomain.DeliveryQueued {
		t.Fatalf("pending no debe mostrar acuses: %+v", v)
	}

	failed := base(notiDomain.StatusFailed)
	msg := "twilio: undelivered (error 63024)"
	failed.ErrorMessage = &msg
	if v := memberNotificationView(failed); v.FailureReason != "El número no tiene WhatsApp o no es válido." {
		t.Fatalf("reason = %q", v.FailureReason)
	}

	held := base(notiDomain.StatusHeld)
	if v := memberNotificationView(held); v.FailureReason == "" || v.DeliveryState != notiDomain.DeliveryHeld {
		t.Fatalf("held view = %+v", v)
	}
}
