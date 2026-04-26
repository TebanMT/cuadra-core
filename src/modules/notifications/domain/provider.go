// Package domain — bounded context `notifications`.
//
// Sesión 7. Owns templates + dispatch (WhatsApp/email/push). Other BCs
// (billing, members, products, reports) push events into the queue; a worker
// goroutine drains the queue calling the providers below.
//
// Provider interfaces live here — concrete impls (Twilio, Resend, Mock) live
// under infraestructure/. The application use cases never import the
// concrete providers, only these interfaces.
package domain

import (
	"context"

	"github.com/google/uuid"
)

// GymWhatsAppConfig is the per-gym credential bundle the WhatsAppProvider
// needs to send. Fields are typed strings so the concrete provider can
// interpret them however it likes (Twilio uses MessagingServiceSID +
// FromNumber; Meta direct uses PhoneNumberID + access token).
type GymWhatsAppConfig struct {
	GymID    uuid.UUID
	Phone    string // E.164, e.g. "+524421234567" — the gym's WhatsApp business number
	SenderID string // provider-side sender / messaging service id
}

// WhatsAppProvider abstracts the Twilio (or future Meta-direct) integration.
// All methods are gym-scoped because Cuadra is multi-tenant: each gym has
// its own approved WhatsApp business number behind the same Cuadra Twilio
// master account.
type WhatsAppProvider interface {
	// SendTemplate fires a pre-approved template message. Returns the
	// provider's message ID (Twilio SID) on success. The provider is
	// responsible for substituting variables in the order Meta/Twilio
	// expects; Cuadra hands them already keyed by name.
	SendTemplate(ctx context.Context, cfg GymWhatsAppConfig, recipient, templateKey string, vars map[string]string) (providerMessageID string, err error)

	// SendFreeform sends an arbitrary text — only valid inside the 24h
	// service window with the recipient. Used for owner-to-member replies
	// (V1.0+); MVP code paths should prefer SendTemplate.
	SendFreeform(ctx context.Context, cfg GymWhatsAppConfig, recipient, text string) (providerMessageID string, err error)

	// RegisterSender starts the onboarding flow for a new gym number
	// (UC-037). Returns the provider's sender id; UI then prompts the
	// owner for the verification code. The exact verification ceremony
	// is provider-specific — for Twilio, the SID is created and the
	// owner finalises in their dashboard.
	RegisterSender(ctx context.Context, phone string) (senderID string, err error)
}

// EmailProvider is the (much smaller) email surface. MVP only the security
// flows in users/ use it — password reset, transfer-ownership OTP. Owner
// alerts that don't fit utility WhatsApp templates also fall through here.
type EmailProvider interface {
	SendTransactional(ctx context.Context, to, subject, body, tag string) (providerMessageID string, err error)
}
