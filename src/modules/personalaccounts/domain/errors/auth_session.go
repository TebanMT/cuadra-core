package errors

import "errors"

var (
	ErrInvalidDevice         = errors.New("equipo personal inválido")
	ErrInvalidSession        = errors.New("sesión personal inválida")
	ErrAuthObjectNotFound    = errors.New("equipo o sesión no encontrado")
	ErrAuthObjectUnavailable = errors.New("equipo o sesión no disponible")
	ErrAuthObjectConflict    = errors.New("equipo o sesión ya registrado")
	ErrAuthSessionScope      = errors.New("ámbito de sesión inválido")
)
