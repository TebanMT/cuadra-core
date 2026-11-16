package errors

import "errors"

var (
	ErrInvalidPhoneChange = errors.New("solicitud de cambio de teléfono inválida")
	ErrPhoneChangeRetry   = errors.New("la identidad cambió durante el bloqueo de teléfono")
)
