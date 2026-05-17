// Package errors holds the sentinel errors of the notifications BC.
// Spanish-language messages; surfaced to the operator/owner via the HTTP
// layer mapping in shared/utils.
//
// UC mappings:
//
//	UC-037 ConnectWhatsApp        -> ErrInvalidPhone, ErrAlreadyConnected, ErrProviderUnavailable
//	UC-038 EnqueueExpiryReminder  -> ErrTemplateNotFound, ErrInvalidVariables
//	UC-038 UpdateTemplate         -> ErrTemplateNotFound, ErrTemplateBodyTooLong, ErrTemplateMissingVar
//	UC-039 EnqueueReceipt         -> ErrRecipientUnreachable
//	UC-040 EnqueueOwnerAlert      -> ErrRecipientUnreachable
//	UC-041 Broadcast              -> ErrBroadcastEmpty, ErrTemplateNotFound
//	dispatch                       -> ErrProviderUnavailable, ErrSendFailedRetry, ErrSendFailedFinal
//	webhooks                       -> ErrInvalidSignature
package errors

import "errors"

var (
	// Generic
	ErrNotificationNotFound = errors.New("notificación no encontrada")
	ErrCrossGym             = errors.New("ese registro no pertenece a tu gimnasio")

	// UC-037 Connect WhatsApp
	ErrInvalidPhone        = errors.New("número de WhatsApp inválido")
	ErrAlreadyConnected    = errors.New("este gimnasio ya tiene un WhatsApp conectado")
	ErrNotConnected        = errors.New("el gimnasio aún no conecta su WhatsApp")
	ErrProviderUnavailable = errors.New("el proveedor de WhatsApp no está disponible")

	// Templates
	ErrTemplateNotFound    = errors.New("plantilla de mensaje no encontrada")
	ErrTemplateBodyTooLong = errors.New("el cuerpo de la plantilla es demasiado largo")
	ErrTemplateBodyEmpty   = errors.New("el cuerpo de la plantilla no puede estar vacío")
	ErrTemplateMissingVar  = errors.New("la plantilla debe incluir todas las variables requeridas")
	ErrInvalidVariables    = errors.New("variables inválidas para esta plantilla")

	// Dispatch
	ErrRecipientUnreachable = errors.New("el destinatario no tiene canal disponible (sin WhatsApp / email)")
	ErrSendFailedRetry      = errors.New("envío falló temporalmente; se reintentará")
	ErrSendFailedFinal      = errors.New("envío falló definitivamente")

	// Broadcast
	ErrBroadcastEmpty   = errors.New("el broadcast no tiene destinatarios")
	ErrBroadcastTooBig  = errors.New("el broadcast excede el máximo permitido en un envío")
	ErrBroadcastMessage = errors.New("el mensaje del broadcast no puede estar vacío")

	// Webhooks
	ErrInvalidSignature = errors.New("firma del webhook inválida")
	ErrInvalidPayload   = errors.New("payload del webhook inválido")

	// Owner-alert config (UC-040 DA-40.1)
	ErrAlertKeyUnknown  = errors.New("clave de alerta desconocida")
	ErrAlertUpdateEmpty = errors.New("nada que actualizar: incluye al menos enabled o body")
)
