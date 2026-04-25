package errors

import "errors"

// Sentinels for the gyms BC. The HTTP layer maps these to status codes via
// CustomError wrapping (NewBusinessError / NewValidationError). Keep messages
// in es-MX for anything that may bubble up to the user.
var (
	ErrGymNotFound          = errors.New("gimnasio no encontrado")
	ErrGymAlreadySetup      = errors.New("este gimnasio ya completó su configuración inicial")
	ErrSetupIncomplete      = errors.New("aún faltan datos de configuración")
	ErrInvalidGymName       = errors.New("nombre del gimnasio inválido")
	ErrInvalidWhatsApp      = errors.New("número de WhatsApp inválido")
	ErrInvalidRFC           = errors.New("RFC con formato inválido")
	ErrInvalidColor         = errors.New("color inválido (hex #RRGGBB)")
	ErrInvalidPaymentMethod = errors.New("método de pago no soportado")
	ErrPaymentMethodsEmpty  = errors.New("debes aceptar al menos un método de pago")
)
