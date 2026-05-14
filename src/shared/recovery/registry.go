package recovery

import (
	"context"
	"errors"
)

// EmailOnlyRegistry is the MVP policy: always pick the EmailChannel
// regardless of user preference, gym tier, or phone availability. WhatsApp
// is not yet a real channel, so trying to pick it would just surface
// ErrWhatsAppNotImplemented at the use-case layer — better to keep the
// policy explicit here.
//
// When fase 2 ships the WhatsApp transport, swap this for a
// TieredRegistry (skeleton below) that prefers WhatsApp on Plus and falls
// back to email — no use-case changes needed.
type EmailOnlyRegistry struct {
	Email *EmailChannel
}

func NewEmailOnlyRegistry(email *EmailChannel) *EmailOnlyRegistry {
	return &EmailOnlyRegistry{Email: email}
}

func (r *EmailOnlyRegistry) Pick(_ context.Context, _, _ string, _ string) (Sender, error) {
	if r.Email == nil {
		return nil, errors.New("recovery: email channel not configured")
	}
	return r.Email, nil
}

// TieredRegistry is the fase-2 shape kept here so the policy lives in one
// file the team can find. Wiring is left to a future change so this stays
// inert in MVP. The selection logic is intentionally explicit:
//
//   1. If the gym is on a tier that includes WhatsApp recovery AND the
//      user has a phone on file, return WhatsApp.
//   2. Else return email.
//
// We do NOT silently fall back from WhatsApp to email when the WhatsApp
// send fails — the user explicitly chose phone-based recovery and the
// app should surface the failure rather than route the reset code to a
// channel the user may not check.
type TieredRegistry struct {
	Email    *EmailChannel
	WhatsApp *WhatsAppChannel
	// IsPlusGym reports whether the gym is on a tier that includes
	// WhatsApp recovery (today: only Plus). Injected so this package
	// doesn't depend on the gyms module.
	IsPlusGym func(ctx context.Context, gymID string) (bool, error)
}

func (r *TieredRegistry) Pick(ctx context.Context, gymID, userID, _ string) (Sender, error) {
	if r.IsPlusGym != nil && r.WhatsApp != nil {
		ok, err := r.IsPlusGym(ctx, gymID)
		if err == nil && ok {
			canWA, err := r.WhatsApp.CanReach(ctx, userID)
			if err == nil && canWA {
				return r.WhatsApp, nil
			}
		}
	}
	if r.Email == nil {
		return nil, errors.New("recovery: email channel not configured")
	}
	return r.Email, nil
}
