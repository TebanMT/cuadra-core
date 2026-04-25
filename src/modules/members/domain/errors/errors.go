package errors

import "errors"

// Sentinel errors for the members BC. Spanish messages are intended for
// surfacing to operators / owners. UC mappings:
//
//	UC-011 MembershipType  -> ErrMembershipTypeNotFound, ErrMembershipTypeNameDuplicate, ErrInvalidMembershipTypeName, ErrInvalidPrice, ErrInvalidDuration, ErrInvalidMaintenanceFreq, ErrMembershipTypeInactive
//	UC-012 CreateMember    -> ErrInvalidPhone, ErrInvalidName, ErrInvalidEmail, ErrInvalidStartDate, ErrPhoneAlreadyExists (warning)
//	UC-013 UpdateMember    -> ErrMemberNotFound, ErrInvalidPhone, ErrInvalidEmail
//	UC-016 ToggleStatus    -> ErrInvalidStatus
//	UC-017 LockExpiry      -> ErrAdjustmentReasonRequired, ErrMembershipNotFound, ErrNoActiveMembership
//	UC-032 AssignPin       -> ErrInvalidPin, ErrPinAlreadyTaken
var (
	// Membership Type
	ErrMembershipTypeNotFound      = errors.New("plan de membresía no encontrado")
	ErrInvalidMembershipTypeName   = errors.New("nombre del plan inválido")
	ErrInvalidPrice                = errors.New("el precio debe ser mayor a cero")
	ErrInvalidDuration             = errors.New("la duración debe ser de al menos 1 día")
	ErrInvalidMaintenanceFreq      = errors.New("frecuencia de mantenimiento inválida")
	ErrMembershipTypeNameDuplicate = errors.New("ya tienes un plan con ese nombre")
	ErrMembershipTypeInactive      = errors.New("este plan está desactivado y no se puede asignar")
	ErrMembershipTypeInUse         = errors.New("este plan tiene socios activos; desactívalo en lugar de borrarlo")

	// Member
	ErrMemberNotFound      = errors.New("socio no encontrado")
	ErrInvalidPhone        = errors.New("el teléfono debe tener 10 dígitos (sin espacios ni guiones)")
	ErrInvalidName         = errors.New("el nombre debe tener entre 3 y 100 caracteres")
	ErrInvalidEmail        = errors.New("ese correo no se ve bien")
	ErrInvalidStartDate    = errors.New("la fecha de inicio no puede ser tan lejana")
	ErrPhoneAlreadyExists  = errors.New("ya existe un socio con este teléfono en tu gimnasio")
	ErrInvalidStatus       = errors.New("estado de socio inválido")
	ErrCrossGym            = errors.New("ese socio no pertenece a tu gimnasio")
	ErrInvalidNotesTooLong = errors.New("las notas son demasiado largas")
	ErrInvalidBirthdate    = errors.New("fecha de nacimiento inválida")

	// Membership
	ErrMembershipNotFound = errors.New("membresía no encontrada")
	ErrNoActiveMembership = errors.New("el socio no tiene membresía vigente")

	// Adjustment
	ErrAdjustmentReasonRequired = errors.New("la razón del ajuste es obligatoria (mínimo 5 caracteres)")
	ErrAdjustmentInvalidDays    = errors.New("la cantidad de días del ajuste no es válida")

	// PIN
	ErrInvalidPin      = errors.New("el PIN debe ser de 4 dígitos")
	ErrPinAlreadyTaken = errors.New("ese PIN ya está en uso por otro socio del gimnasio")
)
