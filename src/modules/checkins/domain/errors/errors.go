// Package errors holds the sentinel errors for the checkins BC. Spanish
// messages are surfaced to operators / kiosko users by the controller layer.
//
// UC mapping:
//
//	UC-029 Checkin por huella   -> ErrFingerprintNotEnrolled, ErrNoFingerprintMatch, ErrReaderNotAvailable
//	UC-030 Checkin manual       -> ErrMemberRequired, ErrOperatorRequired
//	UC-032 Checkin por PIN      -> ErrPinIncorrect, ErrPinTooManyAttempts
//	DA-29.2 Override            -> ErrOverrideReasonTooShort, ErrCannotOverrideAllowed
package errors

import "errors"

var (
	// Generic
	ErrCheckinNotFound     = errors.New("checkin no encontrado")
	ErrMemberRequired      = errors.New("falta el socio del checkin")
	ErrOperatorRequired    = errors.New("este checkin requiere un operador autenticado")
	ErrInvalidMethod       = errors.New("método de checkin inválido")
	ErrUnknownAccessStatus = errors.New("estado de acceso desconocido para este checkin")

	// Fingerprint (UC-029)
	ErrReaderNotAvailable     = errors.New("el lector de huella no está conectado")
	ErrFingerprintNotEnrolled = errors.New("ningún socio tiene huella registrada")
	ErrNoFingerprintMatch     = errors.New("no identifiqué la huella; intenta de nuevo o pasa con recepción")

	// PIN (UC-032)
	ErrPinIncorrect       = errors.New("PIN incorrecto. Intenta de nuevo")
	ErrPinTooManyAttempts = errors.New("demasiados intentos de PIN; espera un minuto y vuelve a intentar")
	ErrPinFormat          = errors.New("el PIN debe ser de 4 dígitos")

	// Override (DA-29.2)
	ErrOverrideReasonTooShort = errors.New("la razón del override debe tener al menos 5 caracteres")
	ErrCannotOverrideAllowed  = errors.New("no se puede aplicar override a un acceso ya permitido")
)
