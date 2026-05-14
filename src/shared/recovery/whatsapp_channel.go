package recovery

import (
	"context"
	"errors"
)

// WhatsAppChannel is the stub seam for fase 2 (Plus tier, ADR-007 wires the
// Twilio provider). The implementation deliberately returns an error today
// so misconfigured deploys can't silently route reset codes into a black
// hole — main.go only registers this channel once the WhatsApp transport
// is actually plumbed.
//
// Fase 2 work (NOT in this change):
//   - Resolve recipient = user.phone, normalize to E.164 (segment is MX so
//     +52 default), reject when missing.
//   - Replace the long reset link with a 6-digit OTP (recovery use case
//     adapts; the channel hint exposes whichever digits to render in the
//     "we sent code to ***5678" UI).
//   - Wire Twilio WhatsApp template (HSM-approved by Meta) via the shared
//     whatsapp transport (already exists for vencimientos / cumpleaños).
//   - Gate on gyms.subscription_plan == "plus" inside the Registry.
type WhatsAppChannel struct {
	// Phone is the user-phone lookup. Injected so the channel doesn't drag
	// in the users repo. Returns ("", false) when the user has no phone.
	Phone func(ctx context.Context, userID string) (string, bool, error)
}

func NewWhatsAppChannel(phone func(ctx context.Context, userID string) (string, bool, error)) *WhatsAppChannel {
	return &WhatsAppChannel{Phone: phone}
}

func (c *WhatsAppChannel) Name() Channel { return ChannelWhatsApp }

func (c *WhatsAppChannel) CanReach(ctx context.Context, userID string) (bool, error) {
	if c.Phone == nil {
		return false, nil
	}
	_, ok, err := c.Phone(ctx, userID)
	return ok, err
}

// ErrWhatsAppNotImplemented documents that the seam exists but the
// transport doesn't ship a real implementation in MVP. The Registry must
// NOT pick this channel until fase 2 swaps in a working Send.
var ErrWhatsAppNotImplemented = errors.New("recovery.whatsapp: not implemented (fase 2)")

func (c *WhatsAppChannel) Send(_ context.Context, _ Payload) (DeliveryResult, error) {
	return DeliveryResult{}, ErrWhatsAppNotImplemented
}
