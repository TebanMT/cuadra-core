package template_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	tpl "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
)

func TestRender_HappyPath(t *testing.T) {
	def := tpl.LookupDefault("expiry_reminder_3d")
	if def == nil {
		t.Fatal("default template missing")
	}
	out, err := tpl.Render(def.Body, map[string]string{
		"member_first_name": "Juan",
		"gym_name":          "Gym Bros",
		"expiry_date":       "12 abr 2026",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "Juan") || !strings.Contains(out, "Gym Bros") || !strings.Contains(out, "12 abr 2026") {
		t.Errorf("rendered output missing variables: %q", out)
	}
	if strings.Contains(out, "{") {
		t.Errorf("placeholder remains: %q", out)
	}
}

func TestRender_MissingVariable(t *testing.T) {
	_, err := tpl.Render("Hola {member_first_name}, {gym_name}", map[string]string{
		"gym_name": "Gym Bros",
	})
	if !errors.Is(err, notiErrors.ErrTemplateMissingVar) {
		t.Errorf("err = %v, want ErrTemplateMissingVar", err)
	}
}

func TestRender_RepeatedPlaceholder(t *testing.T) {
	out, err := tpl.Render("Hola {x}, {x} es tu nombre", map[string]string{"x": "Juan"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != "Hola Juan, Juan es tu nombre" {
		t.Errorf("got %q", out)
	}
}

func TestDefinitionValidate_RequiresAllVariables(t *testing.T) {
	def := tpl.LookupDefault("receipt_membership")
	if def == nil {
		t.Fatal("default template missing")
	}

	good := "Hola {member_first_name}, ${amount} por {membership_type} hasta {expiry_date} — {gym_name}. Comprobante: {receipt_url}"
	if err := def.Validate(good); err != nil {
		t.Fatalf("good body rejected: %v", err)
	}

	missing := "Hola {member_first_name}"
	if err := def.Validate(missing); !errors.Is(err, notiErrors.ErrTemplateMissingVar) {
		t.Errorf("expected ErrTemplateMissingVar, got %v", err)
	}

	if err := def.Validate(""); !errors.Is(err, notiErrors.ErrTemplateBodyEmpty) {
		t.Errorf("expected ErrTemplateBodyEmpty, got %v", err)
	}

	tooLong := strings.Repeat("a", tpl.MaxBodyLen+1)
	if err := def.Validate(tooLong); !errors.Is(err, notiErrors.ErrTemplateBodyTooLong) {
		t.Errorf("expected ErrTemplateBodyTooLong, got %v", err)
	}
}

func TestNewOverride_ValidatesBody(t *testing.T) {
	now := time.Now().UTC()
	gymID := uuid.New()

	good := "Hola {member_first_name} 👋 — vence {expiry_date} ({gym_name})"
	o, err := tpl.NewOverride(uuid.New(), gymID, "expiry_reminder_3d", good, now)
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if !o.Enabled {
		t.Errorf("default Enabled should be true")
	}
	if o.Version != 1 {
		t.Errorf("version = %d", o.Version)
	}

	if _, err := tpl.NewOverride(uuid.New(), gymID, "no_such_key", good, now); !errors.Is(err, notiErrors.ErrTemplateNotFound) {
		t.Errorf("expected ErrTemplateNotFound, got %v", err)
	}
}

func TestOverride_EditAndDisable(t *testing.T) {
	now := time.Now().UTC()
	o, err := tpl.NewOverride(uuid.New(), uuid.New(), "expiry_reminder_today",
		"Hola {member_first_name}, hoy en {gym_name}", now)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := o.Edit("Hola {member_first_name}, te esperamos en {gym_name}", now.Add(time.Minute)); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if o.Version != 2 {
		t.Errorf("version after edit = %d", o.Version)
	}

	o.SetEnabled(false, now.Add(2*time.Minute))
	if o.Enabled {
		t.Errorf("expected disabled")
	}
	if o.Version != 3 {
		t.Errorf("version after disable = %d", o.Version)
	}

	o.SetEnabled(false, now.Add(3*time.Minute))
	if o.Version != 3 {
		t.Errorf("idempotent disable should not bump version")
	}
}
