//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// TestApplyPullChange_NotificationTemplates_AterrizaElToggle — pin de la
// dirección pull del caso 7 (multi-device / full-sync): un override de
// plantilla que baja del cloud debe aterrizar en el SQLite local con el
// switch respetado (bool del wire → INTEGER 0/1). Antes del registro en
// SyncedTables, ApplyPullChange devolvía nil SILENCIOSO para este tipo
// (forward-compat de tipos desconocidos) y el device B nunca se enteraba
// del toggle — este test truena si alguien saca el tipo del registry.
func TestApplyPullChange_NotificationTemplates_AterrizaElToggle(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)

	overrideID := uuid.New()
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	// Payload EXACTO al que emite template_sqlite.enqueueTemplate.
	payload, _ := json.Marshal(map[string]any{
		"id":           overrideID.String(),
		"gym_id":       gymID.String(),
		"version":      2,
		"template_key": "receipt_membership",
		"body":         "",
		"enabled":      false,
		"created_at":   now.Add(-time.Hour).UnixMilli(),
		"updated_at":   now.UnixMilli(),
	})

	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, syncpkg.PullChange{
			EntityType:      "notification_templates",
			EntityID:        overrideID.String(),
			Version:         2,
			Payload:         payload,
			ServerUpdatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	var row struct {
		TemplateKey string `db:"template_key"`
		Enabled     int    `db:"enabled"`
	}
	if err := db.Get(&row, `SELECT template_key, enabled FROM notification_templates WHERE id = ?`, overrideID); err != nil {
		t.Fatalf("el override no aterrizó en SQLite (¿tipo fuera de SyncedTables otra vez?): %v", err)
	}
	if row.TemplateKey != "receipt_membership" || row.Enabled != 0 {
		t.Errorf("row = %+v, want receipt_membership deshabilitado (enabled=0)", row)
	}
}

// TestApplyPullChange_UsersEmailVacio_SePreserva — bug latente hermano del
// de body: users.email es NOT NULL en SQLite con unique PARCIAL
// (WHERE email <> '' — schema 016, diseñado para operadores PIN-only con
// email ""). El apply colapsaba "" → NULL, así que pull-ear un operador
// PIN-only desde el cloud (multi-device fase 2, o full-sync de
// reinstalación) moría con "NOT NULL constraint failed: users.email". El
// projector cloud ya tenía la excepción (notNullStringColumns); éste pinea
// el espejo sidecar (keepEmptyStringColumns).
func TestApplyPullChange_UsersEmailVacio_SePreserva(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)

	opID := uuid.New()
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	payload, _ := json.Marshal(map[string]any{
		"id":            opID.String(),
		"gym_id":        gymID.String(),
		"version":       1,
		"email":         "", // operador PIN-only: sin correo, "" válido
		"password_hash": "x",
		"full_name":     "Operador PIN",
		"role":          "operator",
		"active":        true,
		"created_at":    now.Add(-time.Hour).UnixMilli(),
		"updated_at":    now.UnixMilli(),
	})

	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, syncpkg.PullChange{
			EntityType:      "users",
			EntityID:        opID.String(),
			Version:         1,
			Payload:         payload,
			ServerUpdatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("apply de operador PIN-only: %v", err)
	}
	var email string
	if err := db.Get(&email, `SELECT email FROM users WHERE id = ?`, opID); err != nil {
		t.Fatalf("leer operador: %v", err)
	}
	if email != "" {
		t.Errorf("email = %q, want \"\" preservado (no NULL)", email)
	}
}
