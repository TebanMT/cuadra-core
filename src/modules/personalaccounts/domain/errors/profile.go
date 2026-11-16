package errors

import "errors"

var (
	ErrAccountNotFound          = errors.New("cuenta personal no encontrada")
	ErrAccountUnavailable       = errors.New("la cuenta personal no está disponible")
	ErrInvalidAccount           = errors.New("la cuenta personal no es válida")
	ErrInvalidDisplayName       = errors.New("escribe un nombre de entre 1 y 80 caracteres")
	ErrInvalidWeightUnit        = errors.New("elige kg o lb")
	ErrInvalidProfileRevision   = errors.New("la revisión del perfil no es válida")
	ErrProfileRevisionConflict  = errors.New("el perfil cambió; revisa la versión actual antes de guardar")
	ErrProfileRevisionExhausted = errors.New("no se puede incrementar la revisión del perfil")
)
