package errors

import "errors"

var (
	ErrRecoveryKeyNotFound = errors.New("kit de recuperación no encontrado")
	ErrRecoveryKeyConflict = errors.New("kit de recuperación ya registrado")
	ErrRecoveryKeyState    = errors.New("kit de recuperación no disponible")
)
