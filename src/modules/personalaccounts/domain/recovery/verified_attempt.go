package recovery

import (
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"time"
)

// Verify no kit until the original attempt has directly completed an OTP
// Check. Its matching durable check receipt is a separate persistence guard.
func ValidateVerifiedAttempt(a *attempt.AuthAttempt, now time.Time) error {
	if a == nil || a.Validate() != nil || a.Purpose != attempt.Recovery || a.State != attempt.Verified || a.AccountID != nil || a.ProviderReference == "" || a.ProviderOperation == nil || a.ProviderOperation.Kind != attempt.Check || a.VerifiedAt == nil || now.IsZero() || now.Before(*a.VerifiedAt) || !now.Before(a.ExpiresAt) {
		return personalErrors.ErrRecoveryKeyState
	}
	return nil
}
