package errors

import "errors"

// Sentinel errors for the users BC. Spanish messages are intended for
// surfacing to operators / owners. UC mappings:
//
//	UC-002 Login         -> ErrInvalidCredentials, ErrAccountInactive
//	UC-001 Signup        -> ErrEmailAlreadyExists, ErrInvalidEmail, ErrPasswordTooShort, ErrNameRequired
//	UC-004 Reset         -> ErrInvalidResetToken, ErrResetTokenExpired
//	UC-006 CreateOper    -> ErrEmailAlreadyExists, ErrOperatorLimitReached
//	UC-008 ToggleActive  -> ErrSelfDeactivate, ErrCannotDeactivateOwner
//	UC-010 Transfer      -> ErrInvalidOTP, ErrTargetNotEligible
var (
	ErrUserNotFound          = errors.New("usuario no encontrado")
	ErrEmailAlreadyExists    = errors.New("este correo ya tiene una cuenta")
	ErrInvalidEmail          = errors.New("ese correo no se ve bien")
	ErrPasswordTooShort      = errors.New("la contraseña debe tener al menos 8 caracteres")
	ErrPasswordMismatch      = errors.New("las contraseñas no coinciden")
	ErrNameRequired          = errors.New("el nombre completo es obligatorio")
	ErrNameLooksLikeEmail    = errors.New("ese se ve como un correo. Escribe tu nombre")
	ErrInvalidCredentials    = errors.New("correo o contraseña incorrectos")
	ErrAccountInactive       = errors.New("tu cuenta está desactivada. Contacta al dueño del gimnasio")
	ErrInvalidResetToken     = errors.New("este link ya no es válido")
	ErrResetTokenExpired     = errors.New("este link expiró. Solicita uno nuevo")
	ErrInvalidRole           = errors.New("rol no soportado")
	ErrSelfDeactivate        = errors.New("no puedes desactivar tu propia cuenta")
	ErrCannotDeactivateOwner = errors.New("no puedes desactivar al dueño del gimnasio")
	ErrCrossGym              = errors.New("ese usuario no pertenece a tu gimnasio")
	ErrOperatorLimitReached  = errors.New("alcanzaste el máximo de 10 operadores por gimnasio")
	ErrInvalidOTP            = errors.New("código incorrecto o expirado")
	ErrTargetNotEligible     = errors.New("el usuario seleccionado no existe o no es operador activo de tu gimnasio")
	ErrAlreadyOwner          = errors.New("ese usuario ya es dueño")
	ErrTransferToSelf        = errors.New("no puedes transferir a ti mismo")
	ErrInvalidToken          = errors.New("token inválido")
	ErrInvalidPhone          = errors.New("ese teléfono no se ve bien")
	ErrInvalidPIN            = errors.New("el PIN debe ser de 4 dígitos")
	ErrPINNotSet             = errors.New("este usuario no tiene PIN configurado")
	ErrInvalidPINLogin       = errors.New("PIN incorrecto")
)
