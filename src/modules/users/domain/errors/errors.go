package errors

import "errors"

// Sentinel errors for the users BC. Spanish messages are intended for
// surfacing to operators / owners. UC mappings:
//
//	UC-002 Login         -> ErrInvalidCredentials, ErrAccountInactive
//	UC-001 Signup        -> ErrEmailAlreadyExists, ErrInvalidEmail, ErrPasswordTooShort, ErrNameRequired, ErrLegalNotAccepted
//	UC-004 Reset         -> ErrInvalidResetToken, ErrResetTokenExpired
//	UC-006 CreateOper    -> ErrEmailAlreadyExists, ErrOperatorLimitReached, ErrOperatorLimitStandard
//	UC-008 ToggleActive  -> ErrSelfDeactivate, ErrCannotDeactivateOwner, ErrOperatorLimitReached, ErrOperatorLimitStandard (al reactivar)
//	UC-010 Transfer      -> ErrInvalidOTP, ErrTargetNotEligible
var (
	ErrUserNotFound       = errors.New("usuario no encontrado")
	ErrEmailAlreadyExists = errors.New("este correo ya tiene una cuenta")
	ErrInvalidEmail       = errors.New("ese correo no se ve bien")
	ErrPasswordTooShort   = errors.New("la contraseña debe tener al menos 8 caracteres")
	ErrPasswordMismatch   = errors.New("las contraseñas no coinciden")
	ErrNameRequired       = errors.New("el nombre completo es obligatorio")
	ErrNameLooksLikeEmail = errors.New("ese se ve como un correo. Escribe tu nombre")
	ErrInvalidCredentials = errors.New("correo o contraseña incorrectos")
	ErrAccountInactive    = errors.New("tu cuenta está desactivada. Contacta al dueño del gimnasio")
	// ErrOwnerOnlyLogin — el binario cloud restringe /auth/login al dueño
	// (el dashboard es superficie del dueño). El sidecar nunca lo emite:
	// recepción sí loguea operadores.
	ErrOwnerOnlyLogin = errors.New("el dashboard es del dueño. En el gym, entra con la app de recepción")
	// ErrInvalidRefreshToken — el refresh token venció, ya se usó, se cerró
	// sesión o se cambió la contraseña. El cliente vuelve al login.
	ErrInvalidRefreshToken = errors.New("tu sesión terminó. Vuelve a entrar")
	ErrInvalidResetToken   = errors.New("este link ya no es válido")
	ErrResetTokenExpired   = errors.New("este link expiró. Solicita uno nuevo")
	// ErrLegalNotAccepted — UC-001: el alta del dueño exige aceptar los
	// Términos del Servicio y el Aviso de Privacidad.
	ErrLegalNotAccepted = errors.New("acepta los Términos del Servicio y el Aviso de Privacidad para crear tu cuenta")
	// UC-013 — verificación de correo.
	ErrInvalidVerifyToken    = errors.New("este link de verificación ya no es válido")
	ErrVerifyTokenExpired    = errors.New("este link de verificación expiró. Solicita uno nuevo")
	ErrEmailAlreadyVerified  = errors.New("este correo ya está verificado")
	ErrInvalidRole           = errors.New("rol no soportado")
	ErrSelfDeactivate        = errors.New("no puedes desactivar tu propia cuenta")
	ErrCannotDeactivateOwner = errors.New("no puedes desactivar al dueño del gimnasio")
	ErrCrossGym              = errors.New("ese usuario no pertenece a tu gimnasio")
	// ErrOperatorLimitReached / ErrOperatorLimitStandard: el cupo cuenta sólo
	// operadores ACTIVOS (los desactivados no ocupan lugar; el dueño no
	// cuenta). Aplican al dar de alta y al reactivar un operador.
	ErrOperatorLimitReached = errors.New("alcanzaste el máximo de 10 operadores activos por gimnasio. Desactiva uno para dar acceso a otra persona")
	// ErrOperatorLimitStandard se devuelve cuando el gym intenta crear o
	// reactivar un operador sobre el cap del SKU Standard. El FE lo detecta
	// para mostrar el upsell "mejora a Plus para operadores ilimitados"
	// en lugar del límite absoluto del producto.
	ErrOperatorLimitStandard = errors.New("tu plan Standard incluye 1 operador activo. Desactiva al actual o mejora a Plus para agregar más")
	ErrInvalidOTP            = errors.New("código incorrecto o expirado")
	ErrTargetNotEligible     = errors.New("el usuario seleccionado no existe o no es operador activo de tu gimnasio")
	ErrAlreadyOwner          = errors.New("ese usuario ya es dueño")
	ErrTransferToSelf        = errors.New("no puedes transferir a ti mismo")
	ErrInvalidToken          = errors.New("token inválido")
	ErrInvalidPhone          = errors.New("ese teléfono no se ve bien")
	ErrInvalidPIN            = errors.New("el PIN debe ser de 4 dígitos")
	ErrPINNotSet             = errors.New("este usuario no tiene PIN configurado")
	ErrInvalidPINLogin       = errors.New("PIN incorrecto")
	// El alta y la edición de operadores exigen teléfono: la entrega del PIN
	// va por WhatsApp y el número también funciona como identificador
	// secundario cuando el operador no tiene correo.
	ErrOperatorPhoneRequired = errors.New("el teléfono es obligatorio para operadores")
	// Unicidad por gym (no global): la misma persona puede trabajar en dos
	// gyms distintos con el mismo número.
	ErrPhoneTakenInGym = errors.New("ese teléfono ya está registrado en este gimnasio")
)

// Permisos por operador (UC-007). El dueño siempre tiene todos.
var (
	ErrOwnerHasAllPermissions = errors.New("el dueño siempre tiene todos los permisos")
	ErrUnknownPermission      = errors.New("ese permiso no existe")
)
