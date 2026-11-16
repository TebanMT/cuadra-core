package errors

import "errors"

var (
	ErrDeletionPolicyNotConfigured = errors.New("política de eliminación no configurada")
	ErrInvalidDeletion             = errors.New("solicitud de eliminación inválida")
	ErrDeletionState               = errors.New("estado de eliminación no disponible")
	ErrDeletionResultNotFound      = errors.New("resultado de eliminación no encontrado")
)
