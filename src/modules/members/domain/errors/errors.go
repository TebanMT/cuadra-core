package errors

import "errors"

var (
	ErrMembershipTypeNotFound      = errors.New("plan de membresía no encontrado")
	ErrInvalidMembershipTypeName   = errors.New("nombre del plan inválido")
	ErrInvalidPrice                = errors.New("el precio debe ser mayor a cero")
	ErrInvalidDuration             = errors.New("la duración debe ser de al menos 1 día")
	ErrInvalidMaintenanceFreq      = errors.New("frecuencia de mantenimiento inválida")
	ErrMembershipTypeNameDuplicate = errors.New("ya tienes un plan con ese nombre")
)
