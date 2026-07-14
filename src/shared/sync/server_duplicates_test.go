//go:build server

package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// El mapper es la frontera entre "SQLSTATE 23505 crudo" y el mensaje que el
// operador ve en el indicador del desktop. Estos tests pinean:
//   - constraint conocido + valor extraíble → mensaje dedicado con el valor
//   - constraint conocido sin valor → generic de la regla
//   - constraint desconocido → fallback con el nombre del índice
//   - errores que NO son unique violation → ok=false (siguen el camino
//     rejected_internal_error de siempre)
//
// El round-trip real (error saliendo de GORM/pgx a través del UoW) lo cubre
// server_duplicates_integration_test.go.
func TestMapUniqueViolation_KnownConstraintWithValue(t *testing.T) {
	item := PushItem{Payload: json.RawMessage(`{"name":"Mensual"}`)}
	// Envuelto como viaja en la vida real (el projector/UoW pueden agregar
	// contexto con %w) — errors.As debe encontrar el PgError igual.
	err := fmt.Errorf("projector membership_types: %w",
		&pgconn.PgError{Code: "23505", ConstraintName: "uq_membership_types_gym_name"})

	msg, ok := mapUniqueViolation(err, item)
	if !ok {
		t.Fatalf("ok=false, want true")
	}
	if !strings.Contains(msg, `Ya existe un plan llamado "Mensual"`) {
		t.Errorf("mensaje sin el valor del payload: %q", msg)
	}
	if !strings.Contains(msg, "Renombra") {
		t.Errorf("mensaje sin la acción de salida: %q", msg)
	}
}

func TestMapUniqueViolation_ProductsAndPromos(t *testing.T) {
	cases := []struct {
		constraint string
		payload    string
		want       string
	}{
		{"uq_products_gym_name", `{"name":"Agua Ciel 1L"}`, `Ya existe un producto llamado "Agua Ciel 1L"`},
		{"uq_promotions_gym_code", `{"code":"VERANO25"}`, `Ya existe una promoción con el código "VERANO25"`},
		{"uq_users_email", `{"email":"dueno@gym.mx"}`, `El correo "dueno@gym.mx" ya está registrado`},
		{"uq_payments_gym_folio", `{"folio":"PAGO/123"}`, `Ya existe un pago con el folio "PAGO/123"`},
	}
	for _, c := range cases {
		item := PushItem{Payload: json.RawMessage(c.payload)}
		err := &pgconn.PgError{Code: "23505", ConstraintName: c.constraint}
		msg, ok := mapUniqueViolation(err, item)
		if !ok {
			t.Errorf("%s: ok=false", c.constraint)
			continue
		}
		if !strings.Contains(msg, c.want) {
			t.Errorf("%s: msg=%q, want fragmento %q", c.constraint, msg, c.want)
		}
	}
}

func TestMapUniqueViolation_KnownConstraintWithoutValue(t *testing.T) {
	// Payload sin el campo (o no-string) → generic de la regla, no un
	// Sprintf con "%!s(MISSING)".
	item := PushItem{Payload: json.RawMessage(`{"price":500}`)}
	err := &pgconn.PgError{Code: "23505", ConstraintName: "uq_membership_types_gym_name"}
	msg, ok := mapUniqueViolation(err, item)
	if !ok {
		t.Fatalf("ok=false, want true")
	}
	if strings.Contains(msg, "%") || strings.Contains(msg, "MISSING") {
		t.Errorf("mensaje con placeholder sin resolver: %q", msg)
	}
	if !strings.Contains(msg, "Ya existe un plan") {
		t.Errorf("mensaje inesperado: %q", msg)
	}
}

func TestMapUniqueViolation_UnknownConstraintFallback(t *testing.T) {
	item := PushItem{Payload: json.RawMessage(`{}`)}
	err := &pgconn.PgError{Code: "23505", ConstraintName: "uq_algo_nuevo_sin_regla"}
	msg, ok := mapUniqueViolation(err, item)
	if !ok {
		t.Fatalf("ok=false, want true")
	}
	if !strings.Contains(msg, "uq_algo_nuevo_sin_regla") {
		t.Errorf("fallback sin el nombre del constraint (necesario para soporte): %q", msg)
	}
	if !strings.Contains(msg, "ya existe en la nube") {
		t.Errorf("fallback sin explicación: %q", msg)
	}
}

func TestMapUniqueViolation_NotADuplicate(t *testing.T) {
	item := PushItem{Payload: json.RawMessage(`{"name":"x"}`)}
	for _, err := range []error{
		errors.New("plain error"),
		&pgconn.PgError{Code: "23503", ConstraintName: "fk_members_gym"}, // FK violation
		&pgconn.PgError{Code: "23502"},                                   // NOT NULL
		fmt.Errorf("wrap: %w", errors.New("db down")),
	} {
		if msg, ok := mapUniqueViolation(err, item); ok {
			t.Errorf("err=%v clasificado como duplicado: %q", err, msg)
		}
	}
}
