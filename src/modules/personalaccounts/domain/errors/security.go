package errors

import "errors"

var (
	ErrInvalidAccessToken           = errors.New("la credencial personal no es válida")
	ErrInvalidProof                 = errors.New("la prueba del dispositivo no es válida")
	ErrInvalidRecoveryKey           = errors.New("el kit de recuperación no es válido")
	ErrInvalidEnvelope              = errors.New("no se pudo abrir el contenido protegido")
	ErrInvalidSecurityConfiguration = errors.New("la configuración de seguridad personal no es válida")
)
