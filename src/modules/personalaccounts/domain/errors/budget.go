package errors

import "errors"

var (
	ErrBudgetConfiguration = errors.New("límites de verificación incompletos")
	ErrBudgetInput         = errors.New("reserva de verificación inválida")
	ErrBudgetCorrupt       = errors.New("estado de límite de verificación inválido")
	ErrBudgetExceeded      = errors.New("límite de verificación alcanzado")
	ErrBudgetBlocked       = errors.New("verificación bloqueada temporalmente")
)
