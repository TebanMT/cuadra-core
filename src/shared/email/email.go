// Package email is a thin transport interface. MVP implementation just logs
// to stdout (the spec keeps Resend/SES out of Sesión 1). Use cases depend on
// Sender; main.go picks the impl by env var.
package email

import (
	"context"
	"log"
)

type Message struct {
	To      string
	Subject string
	Body    string
	// Tag is a short label (e.g. "password_reset", "transfer_otp") used for
	// log grep. Not sent to the recipient.
	Tag string
}

type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// StdoutSender writes a one-line summary instead of sending. Default for dev
// and tests. Production wires Resend / SES under the same interface.
type StdoutSender struct{}

func NewStdoutSender() *StdoutSender { return &StdoutSender{} }

func (StdoutSender) Send(_ context.Context, msg Message) error {
	log.Printf("[email] tag=%s to=%s subject=%q body=%q", msg.Tag, msg.To, msg.Subject, msg.Body)
	return nil
}
