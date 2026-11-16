package errors

import "errors"

var (
	ErrVerificationConfiguration = errors.New("configuración de verificación incompleta")
	ErrVerificationInput         = errors.New("solicitud de verificación inválida")
	ErrVerificationUncertain     = errors.New("resultado de verificación incierto")
	ErrVerificationUnavailable   = errors.New("proveedor de verificación no disponible")
	ErrVerificationMissing       = errors.New("verificación no disponible para consulta")
	ErrVerificationRateLimited   = errors.New("límite temporal de verificación")
	ErrVerificationCheckLimit    = errors.New("intentos de verificación agotados")
	ErrVerificationRejected      = errors.New("solicitud rechazada por el proveedor de verificación")
)
