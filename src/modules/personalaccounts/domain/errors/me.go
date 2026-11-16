package errors

import "errors"

var (
	ErrInvalidMe                   = errors.New("la vista de cuenta personal no es válida")
	ErrInvalidRecoveryConfirmation = errors.New("la confirmación del kit no es válida")
)
