package errors

import "errors"

var (
	ErrAccessIdentityConfiguration = errors.New("configuración de identidad inválida")
	ErrAccessIdentityScope         = errors.New("ámbito de acceso inválido")
	ErrAccessIdentityUnavailable   = errors.New("no fue posible continuar el acceso")
)
