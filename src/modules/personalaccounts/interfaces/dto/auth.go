package dto

import (
	"encoding/base64"
	"math"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
)

// AuthAttemptResponse is the public Attempt schema. Never serialize the domain
// attempt: it contains server-only hashes, provider references and candidates.
type AuthAttemptResponse struct {
	ID                 string `json:"id"`
	State              string `json:"state"`
	DestinationMasked  string `json:"destination_masked"`
	Nonce              string `json:"nonce"`
	ExpiresAt          string `json:"expires_at"`
	ResendAfterSeconds int    `json:"resend_after_seconds"`
}

func AuthAttemptFromDomain(a *attempt.AuthAttempt, now time.Time, cooldown time.Duration) AuthAttemptResponse {
	wait := 0
	if a.ProviderOperation != nil {
		remaining := a.ProviderOperation.StartedAt.Add(cooldown).Sub(now)
		if remaining > 0 {
			wait = int(math.Ceil(remaining.Seconds()))
		}
	}
	if wait > 3600 {
		wait = 3600
	}
	state := a.State
	if !now.Before(a.ExpiresAt) && state != attempt.Consumed && state != attempt.Cancelled {
		state = attempt.Expired
	}
	phone := a.DestinationE164
	masked := "•••• " + phone[len(phone)-4:]
	return AuthAttemptResponse{ID: a.ID.String(), State: string(state), DestinationMasked: masked, Nonce: base64.RawURLEncoding.EncodeToString(a.Nonce[:]), ExpiresAt: a.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z"), ResendAfterSeconds: wait}
}
