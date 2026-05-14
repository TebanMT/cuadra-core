// Package recovery is the abstraction behind UC-004 (forgot-password) and
// any future flow that needs to deliver a single-use code or link to a
// user's verified contact channel (WhatsApp, SMS, email).
//
// MVP scope: only the EmailChannel is wired. The WhatsApp channel is
// scaffolded as a stub so the seams are visible — when Plus adds
// WhatsApp-driven recovery in fase 2, the use case won't need to grow a
// new dependency, just swap the registry's default for the gym tier.
//
// Why a channel abstraction at all (vs. just adding a second sender):
//   - Recovery has channel-specific UX. Email gets a long link the user
//     clicks; WhatsApp gets a 6-digit OTP the user types back. The
//     payload shape differs, not just the transport.
//   - Per-gym selection is dynamic. The same user can prefer email today
//     and WhatsApp tomorrow once the operator types their phone into
//     /auth/me. The use case shouldn't have to know which transport is
//     reachable — it asks the registry "the best channel for this user
//     right now" and trusts the answer.
//
// ADR reference: pending ADR-014 "Pluggable recovery channels".
package recovery

import "context"

// Channel enumerates the supported recovery transports. Keep this list
// small and well-known — drift would break the registry's selection rules.
type Channel string

const (
	ChannelEmail    Channel = "email"
	ChannelWhatsApp Channel = "whatsapp"
)

// Payload is what the channel sends to the user. The use case fills in
// the high-entropy Token; channel implementations decide whether to wrap
// it in a URL (email) or render it as a short code (WhatsApp OTP).
type Payload struct {
	// UserID + GymID identify the recipient. Channels resolve their own
	// addressable target (email vs. phone) from the user record so the
	// caller doesn't have to know which one applies.
	UserID string
	GymID  string

	// Token is the single-use secret the user will present back to confirm
	// recovery. Use cases generate it; channels MUST NOT mutate it.
	Token string

	// Reason is a short tag for logs / audit ("password_reset",
	// "transfer_otp"). Not shown to the user.
	Reason string

	// Recipient is the resolved address (email or E.164 phone). The use
	// case populates this from the user record so the channel impl stays
	// pure transport — no User repository dependency leaks into channels.
	Recipient string

	// LinkBaseURL is the public origin (e.g. https://entinta.mx) the email
	// channel uses to build the click-through. Ignored by WhatsApp.
	LinkBaseURL string
}

// DeliveryResult lets the channel report back the user-visible identifier
// for the delivery (e.g. last 4 digits of the SMS-routed phone) so the FE
// can render a "we sent it to ***5678" hint without exposing the full
// address. Empty string = no hint.
type DeliveryResult struct {
	// Hint is the channel's tail (e.g. last 4 digits, masked email) for
	// UI confirmation. Optional.
	Hint string
}

// Channel is the contract each transport implements.
type Sender interface {
	// Send dispatches the payload over this channel. Returns nil on
	// successful enqueue (not necessarily user receipt — most providers
	// are fire-and-forget at this layer).
	Send(ctx context.Context, p Payload) (DeliveryResult, error)

	// CanReach reports whether the channel has a valid address for the
	// user identified by the payload. The registry uses this to pick a
	// fallback (e.g. WhatsApp first, email if phone missing).
	CanReach(ctx context.Context, userID string) (bool, error)

	// Name returns the Channel constant this impl serves. Used by the
	// registry's logging + audit tags.
	Name() Channel
}

// Registry chooses which channel to use for a given (gym, user, reason).
// Implementations are responsible for the policy ("if user has phone AND
// gym is on Plus → WhatsApp, else email"). The use case calls Pick once
// per recovery request and uses the returned Sender.
type Registry interface {
	Pick(ctx context.Context, gymID, userID string, reason string) (Sender, error)
}
