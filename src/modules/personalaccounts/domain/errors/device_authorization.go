package errors

import "errors"

var (
	ErrInvalidDeviceAuthorization  = errors.New("solicitud de autorización no válida")
	ErrDeviceAuthorizationNotFound = errors.New("solicitud de autorización no encontrada")
	ErrDeviceAuthorizationState    = errors.New("solicitud de autorización no disponible")
	ErrDeviceAuthorizationExpired  = errors.New("solicitud de autorización vencida")
)
