// Package errors holds the sentinel errors of the promotions BC. Spanish
// messages (tuteo mexicano) — se muestran al operador / dueño.
package errors

import "errors"

var (
	// CRUD / lookup
	ErrPromotionNotFound      = errors.New("promoción no encontrada")
	ErrPromotionCodeNotFound  = errors.New("no encontramos una promoción con ese código")
	ErrPromotionCodeDuplicate = errors.New("ya tienes una promoción con ese código")
	ErrCrossGym               = errors.New("esa promoción no pertenece a tu gimnasio")

	// Form / dominio
	ErrInvalidPromotionName     = errors.New("el nombre de la promoción debe tener entre 3 y 100 caracteres")
	ErrInvalidPromotionKind     = errors.New("tipo de promoción inválido")
	ErrInvalidAppliesTo         = errors.New("el campo \"aplica a\" debe ser membership, sale o any")
	ErrInvalidPromotionValue    = errors.New("el valor de la promoción no es válido para este tipo")
	ErrInvalidPromotionDates    = errors.New("la fecha de fin debe ser igual o posterior a la de inicio")
	ErrInvalidMaxUses           = errors.New("los topes de uso deben ser mayores a cero (o vacíos)")
	ErrInvalidCompanionCount    = errors.New("la cantidad de membresías de regalo debe ser al menos 1")
	ErrCompanionMembersRequired = errors.New("tienes que elegir a quién regalarle la membresía")
	ErrCompanionMembersMismatch = errors.New("la cantidad de socios elegidos no coincide con la promoción")

	// Aplicación
	ErrPromotionInactive              = errors.New("esta promoción está desactivada")
	ErrPromotionNotYetValid           = errors.New("esta promoción todavía no está vigente")
	ErrPromotionExpired               = errors.New("esta promoción ya expiró")
	ErrPromotionUsageLimitExceeded    = errors.New("esta promoción ya alcanzó su tope de usos")
	ErrPromotionUsageLimitPerMember   = errors.New("este socio ya usó esta promoción el máximo de veces")
	ErrPromotionNotApplicableToTarget = errors.New("esta promoción no aplica a este tipo de cobro")
	ErrPromotionRequiresMember        = errors.New("esta promoción requiere asociar al socio que la usa")
)
