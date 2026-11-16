package errors

import "errors"

var (
	ErrInvalidAuthAttempt    = errors.New("el intento de autenticación no es válido")
	ErrAuthAttemptNotFound   = errors.New("intento de autenticación no encontrado")
	ErrAuthAttemptConflict   = errors.New("el intento de autenticación cambió")
	ErrAuthAttemptExpired    = errors.New("el intento de autenticación venció")
	ErrAuthAttemptState      = errors.New("el intento no permite esta operación")
	ErrAuthAttemptOperation  = errors.New("la operación del proveedor no coincide con el intento")
	ErrAuthAttemptUncertain  = errors.New("el resultado de la verificación todavía es incierto")
	ErrAuthAttemptCheckLimit = errors.New("se alcanzó el límite de comprobaciones del intento")
	ErrAuthAttemptCooldown   = errors.New("la verificación todavía debe esperar")
)
