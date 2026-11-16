package errors

import "errors"

var (
	ErrRecoveryIdentityRetry    = errors.New("la identidad cambió durante la recuperación")
	ErrRecoveryPhoneUnavailable = errors.New("el teléfono no está disponible para recuperación")
)
