package errors

import "errors"

var (
	ErrVaultNotFound = errors.New("almacén cifrado no encontrado")
	ErrVaultConflict = errors.New("almacén cifrado ya registrado")
	ErrVaultState    = errors.New("almacén cifrado no disponible")
)
