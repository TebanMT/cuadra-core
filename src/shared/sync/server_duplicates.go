//go:build server

package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Este archivo traduce violaciones de unique constraint (SQLSTATE 23505)
// del push a rechazos LEGIBLES y accionables. Sin él, el item queda
// rejected_internal_error con el error crudo de Postgres ("duplicate key
// value violates unique constraint …") reintentando para siempre — el
// operador ve jerga en inglés y no tiene idea de que la salida es
// renombrar su registro.
//
// El mapa se indexa por el NOMBRE del constraint/índice tal como vive en
// db_migrations/postgres/. Si agregas un unique index a una tabla
// sincronizada, agrega aquí su regla — el fallback genérico cubre el
// olvido, pero el mensaje dedicado es el que le dice al operador QUÉ
// registro editar.
//
// Nota: varios 23505 potenciales NO llegan aquí porque su projector los
// resuelve solo (projectMember reasigna member_number, projectMembership
// desaloja el slot activo, projectTemplateOverride el suyo). Si uno de
// esos escapa igual, cae al fallback — señal de bug, no de flujo normal.

// dupRule — cómo redactar el rechazo para un constraint conocido.
type dupRule struct {
	// payloadField: campo del payload cuyo valor personaliza el mensaje
	// (name, code, folio…). Vacío = usar siempre generic.
	payloadField string
	// withValue: mensaje con %s para el valor del payload.
	withValue string
	// generic: mensaje cuando el valor no se pudo extraer.
	generic string
}

var dupRules = map[string]dupRule{
	"uq_membership_types_gym_name": {
		payloadField: "name",
		withValue:    "Ya existe un plan llamado \"%s\" en la nube (quizá creado desde otro equipo). Renombra el tuyo para destrabar la sincronización.",
		generic:      "Ya existe un plan con ese nombre en la nube. Renombra el tuyo para destrabar la sincronización.",
	},
	"uq_products_gym_name": {
		payloadField: "name",
		withValue:    "Ya existe un producto llamado \"%s\" en la nube (quizá creado desde otro equipo). Renombra el tuyo para destrabar la sincronización.",
		generic:      "Ya existe un producto con ese nombre en la nube. Renombra el tuyo para destrabar la sincronización.",
	},
	"uq_promotions_gym_code": {
		payloadField: "code",
		withValue:    "Ya existe una promoción con el código \"%s\" en la nube. Cambia el código de la tuya para destrabar la sincronización.",
		generic:      "Ya existe una promoción con ese código en la nube. Cambia el código de la tuya para destrabar la sincronización.",
	},
	"uq_users_email": {
		payloadField: "email",
		withValue:    "El correo \"%s\" ya está registrado por otro usuario en la nube. Usa otro correo.",
		generic:      "Ese correo ya está registrado por otro usuario en la nube. Usa otro correo.",
	},
	"uq_users_gym_phone": {
		payloadField: "phone",
		withValue:    "El teléfono \"%s\" ya está registrado por otro usuario del gimnasio. Usa otro teléfono.",
		generic:      "Ese teléfono ya está registrado por otro usuario del gimnasio. Usa otro teléfono.",
	},
	"uq_members_gym_folio": {
		payloadField: "folio",
		withValue:    "Ya existe un socio con el folio \"%s\" en la nube (quizá dado de alta desde otro equipo).",
		generic:      "Ya existe un socio con ese folio en la nube (quizá dado de alta desde otro equipo).",
	},
	"uq_payments_gym_folio": {
		payloadField: "folio",
		withValue:    "Ya existe un pago con el folio \"%s\" en la nube (quizá registrado desde otro equipo).",
		generic:      "Ya existe un pago con ese folio en la nube (quizá registrado desde otro equipo).",
	},
	"uq_gyms_rfc": {
		payloadField: "rfc",
		withValue:    "El RFC \"%s\" ya está registrado por otro gimnasio en Tinta.",
		generic:      "Ese RFC ya está registrado por otro gimnasio en Tinta.",
	},
	"uq_gyms_whatsapp": {
		payloadField: "whatsapp",
		withValue:    "El WhatsApp \"%s\" ya está registrado por otro gimnasio en Tinta.",
		generic:      "Ese WhatsApp ya está registrado por otro gimnasio en Tinta.",
	},
	"uq_challenge_categories_name": {
		payloadField: "name",
		withValue:    "Ya existe una categoría llamada \"%s\" en ese reto. Renombra la tuya.",
		generic:      "Ya existe una categoría con ese nombre en ese reto. Renombra la tuya.",
	},
	"uq_participants_challenge_member": {
		withValue: "",
		generic:   "Ese socio ya está inscrito en el reto (quizá lo inscribieron desde otro equipo).",
	},
}

// mapUniqueViolation revisa si err (la cadena que salió del UoW.Command del
// push) es una unique violation de Postgres y, de serlo, devuelve el mensaje
// en español para el operador. ok=false → no era 23505, el caller conserva
// su manejo de rejected_internal_error.
func mapUniqueViolation(err error, item PushItem) (string, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return "", false
	}
	rule, known := dupRules[pgErr.ConstraintName]
	if !known {
		// Constraint sin regla dedicada: mensaje genérico con el nombre del
		// índice — suficiente para que soporte ubique la colisión sin pedir
		// logs, y para que el sidecar lo clasifique como duplicate.
		constraint := pgErr.ConstraintName
		if constraint == "" {
			constraint = "índice único desconocido"
		}
		return fmt.Sprintf(
			"Este cambio choca con un dato que ya existe en la nube (%s). Edita el registro en este equipo para resolver el duplicado.",
			constraint), true
	}
	if rule.payloadField != "" {
		if v := stringFieldFromPayload(item.Payload, rule.payloadField); v != "" {
			return fmt.Sprintf(rule.withValue, v), true
		}
	}
	return rule.generic, true
}

// stringFieldFromPayload extrae un campo string del payload del push item.
// Best-effort: payload no-JSON o campo ausente/no-string devuelven "".
func stringFieldFromPayload(payload json.RawMessage, field string) string {
	var pl map[string]any
	if err := json.Unmarshal(payload, &pl); err != nil {
		return ""
	}
	if s, ok := pl[field].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}
