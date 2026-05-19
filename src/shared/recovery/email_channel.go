package recovery

import (
	"context"

	"github.com/cuadra/cuadra-core/src/shared/email"
)

// EmailChannel adapts shared/email.Sender to the recovery.Sender contract.
// Subject + body are minimal on purpose — UC-004's existing copy is the
// canonical version and the use case keeps owning it; this adapter is the
// transport, not the template.
type EmailChannel struct {
	Sender email.Sender
}

func NewEmailChannel(s email.Sender) *EmailChannel { return &EmailChannel{Sender: s} }

func (c *EmailChannel) Name() Channel { return ChannelEmail }

func (c *EmailChannel) CanReach(_ context.Context, _ string) (bool, error) {
	// Every signup requires an email address, so this is always true at
	// the channel level. A Recipient may still be empty at the call site
	// (user soft-deleted, etc.) — that's caught in Send below.
	return true, nil
}

func (c *EmailChannel) Send(ctx context.Context, p Payload) (DeliveryResult, error) {
	if p.Recipient == "" {
		// Belt + suspenders: the registry should have filtered this out,
		// but if a caller invoked Send directly we'd rather no-op than
		// log a leaky message.
		return DeliveryResult{}, nil
	}
	link := p.LinkBaseURL + "/auth/reset-password?token=" + p.Token
	return DeliveryResult{Hint: maskEmail(p.Recipient)}, c.Sender.Send(ctx, email.Message{
		To:      p.Recipient,
		Subject: "Recupera tu contraseña de Tinta",
		Body:    link,
		Tag:     p.Reason,
	})
}

// maskEmail returns a UI-safe hint like "es***@gym.com" — used by the FE
// after forgot-password to confirm "we sent it to ***" without echoing
// the full address (which would let an attacker enumerate by guessing
// emails). Empty input → empty hint.
func maskEmail(addr string) string {
	at := -1
	for i, r := range addr {
		if r == '@' {
			at = i
			break
		}
	}
	if at <= 0 {
		return ""
	}
	local := addr[:at]
	domain := addr[at:]
	if len(local) <= 2 {
		return local + "***" + domain
	}
	return local[:2] + "***" + domain
}
