package event_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/notifications/domain/event"
)

// El invariante del dominio: RawPayload SIEMPRE es JSON válido, porque la
// columna es JSONB NOT NULL (postgres) / TEXT CHECK json_valid (sqlite).
// Bug jul-2026: el form-urlencoded crudo de Twilio guardado tal cual
// rollbackeaba el INSERT (22P02) y el webhook entero devolvía 500.

func TestNewStatusEvent_FormBodyNoJSON_SeEnvuelve(t *testing.T) {
	raw := []byte("ChannelPrefix=whatsapp&MessageStatus=sent&MessageSid=SM123&To=whatsapp%3A%2B5214421234567")
	ev := event.NewStatusEvent(uuid.New(), nil, nil, "SM123", "sent", nil, nil, raw, time.Now().UTC())

	if !json.Valid(ev.RawPayload) {
		t.Fatalf("RawPayload no es JSON válido: %q", ev.RawPayload)
	}
	var decoded map[string]string
	if err := json.Unmarshal(ev.RawPayload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["raw"] != string(raw) {
		t.Errorf("el payload original no se preservó: %v", decoded)
	}
}

func TestNewStatusEvent_JSONValido_SeGuardaTalCual(t *testing.T) {
	raw := []byte(`{"MessageStatus":"delivered","MessageSid":"SM123"}`)
	ev := event.NewStatusEvent(uuid.New(), nil, nil, "SM123", "delivered", nil, nil, raw, time.Now().UTC())
	if string(ev.RawPayload) != string(raw) {
		t.Errorf("RawPayload = %q, want tal cual %q", ev.RawPayload, raw)
	}
}

func TestNewStatusEvent_PayloadVacio_ObjetoVacio(t *testing.T) {
	ev := event.NewStatusEvent(uuid.New(), nil, nil, "SM123", "queued", nil, nil, nil, time.Now().UTC())
	if string(ev.RawPayload) != "{}" {
		t.Errorf("RawPayload = %q, want {} (columna NOT NULL)", ev.RawPayload)
	}
}

func TestNewIncomingEvent_FormBodyNoJSON_SeEnvuelve(t *testing.T) {
	raw := []byte("From=whatsapp%3A%2B5214421234567&Body=BAJA&MessageSid=SM456")
	ev := event.NewIncomingEvent(uuid.New(), nil, "SM456", raw, time.Now().UTC())
	if !json.Valid(ev.RawPayload) {
		t.Fatalf("RawPayload no es JSON válido: %q", ev.RawPayload)
	}
}
