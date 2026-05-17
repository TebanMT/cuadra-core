// Package errors centralises sentinel errors for the challenges (retos)
// bounded context. UC mappings (per the spec / prompt):
//
//	UC-Reto-001 CreateChallenge      -> ErrInvalidDates, ErrNameRequired
//	UC-Reto-002 EditChallengeConfig  -> ErrConfigLocked, ErrChallengeNotFound
//	UC-Reto-003 TransitionStatus     -> ErrInvalidStatusTransition, ErrNoCategories
//	UC-Reto-004 AddParticipant       -> ErrCategoryMismatch, ErrAlreadyParticipating,
//	                                    ErrChallengeNotRegistering
//	UC-Reto-005 CaptureMeasurement   -> ErrMeasurementOutOfWindow, ErrMomentInvalid,
//	                                    ErrParticipantInactive
//	UC-Reto-006 DeleteCategory       -> ErrCategoryHasParticipants
//	UC-Reto-007 RemoveParticipant    -> ErrParticipantHasMeasurements
package errors

import "errors"

// Generic (not found / not authorized).
var (
	ErrChallengeNotFound   = errors.New("reto no encontrado")
	ErrCategoryNotFound    = errors.New("categoría no encontrada")
	ErrParticipantNotFound = errors.New("participante no encontrado")
	ErrMeasurementNotFound = errors.New("medición no encontrada")
	ErrCrossGym            = errors.New("ese reto no pertenece a tu gimnasio")
)

// Challenge creation / configuration.
var (
	ErrNameRequired            = errors.New("el nombre del reto es obligatorio")
	ErrInvalidDates            = errors.New("las fechas no son válidas (T₀ debe ser antes que T₁ y el cierre)")
	ErrInvalidConfig           = errors.New("la configuración del reto tiene un valor fuera de rango")
	ErrConfigLocked            = errors.New("ya hay mediciones capturadas; no se puede editar la configuración del reto")
	ErrInvalidStatusTransition = errors.New("ese cambio de estado no está permitido")
	ErrNoCategories            = errors.New("agrega al menos una categoría antes de abrir inscripciones")
	ErrChallengeNotRegistering = errors.New("el reto no está aceptando inscripciones")
)

// Categories.
var (
	ErrCategoryNameRequired    = errors.New("el nombre de la categoría es obligatorio")
	ErrCategoryNameDuplicated  = errors.New("ya existe una categoría con ese nombre en este reto")
	ErrCategoryHasParticipants = errors.New("esta categoría tiene participantes y no se puede borrar")
)

// Participants.
var (
	ErrAlreadyParticipating       = errors.New("este socio ya está inscrito en el reto")
	ErrCategoryMismatch           = errors.New("la categoría no pertenece a este reto")
	ErrParticipantInactive        = errors.New("este participante no está activo en el reto")
	ErrParticipantHasMeasurements = errors.New("este participante tiene mediciones; márcalo como retirado en lugar de borrar")
	ErrParticipantWrongChallenge  = errors.New("el participante no pertenece a este reto")
)

// Measurements.
var (
	ErrMomentInvalid          = errors.New("momento inválido (usa t0, t1 o intermediate)")
	ErrMeasurementOutOfWindow = errors.New("esta medición no está dentro de la ventana permitida del reto")
	ErrBodyFatOutOfRange      = errors.New("el % de grasa corporal está fuera del rango fisiológico (3% — 60%)")
	ErrRepsOutOfRange         = errors.New("las repeticiones de un test de fuerza submáximo deben estar entre 1 y 15")
	ErrWeightNonPositive      = errors.New("el peso debe ser mayor a cero")
)
