// Package whatsapp wraps WhatsApp message dispatch. MVP impl logs to stdout;
// V1.0 will swap in Twilio (ADR-007). Sesión 1 only uses this from UC-006/UC-009
// to deliver temp passwords if the flow ever wants to (currently the owner
// shares them verbally — see UC-006 design note).
package whatsapp

import (
	"context"
	"log"
)

type Message struct {
	To       string // E.164: +52442...
	Template string // template_key, e.g. "operator_temp_password"
	Vars     map[string]string
}

type Sender interface {
	Send(ctx context.Context, msg Message) error
}

type StdoutSender struct{}

func NewStdoutSender() *StdoutSender { return &StdoutSender{} }

func (StdoutSender) Send(_ context.Context, msg Message) error {
	log.Printf("[whatsapp] to=%s template=%s vars=%v", msg.To, msg.Template, msg.Vars)
	return nil
}
