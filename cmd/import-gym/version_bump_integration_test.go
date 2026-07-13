//go:build server && integration

package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// Pin de la SEGUNDA vuelta del incidente ×100 (jul-2026): el fix #15 dejó el
// journal en pesos, pero re-correr `import-gym --reset` re-emitía TODO con
// version=1 — y el LWW del apply del sidecar (excluded.version > version)
// descarta versiones no-mayores, así que un sidecar que ya había aterrizado
// los payloads envenenados NO se corregía con el pull: la recuperación
// dependía de un wipe manual de AppData perfecto (y de que no hubiera un
// segundo equipo en el gym). nextImportVersion emite por encima de todo lo
// previamente journaleado para que los sidecars se auto-corrijan.
//
// Run: DATABASE_URL=… go test -tags 'server integration' ./cmd/import-gym/
func TestReimportBumpsVersionsAboveJournal(t *testing.T) {
	db := testutil.OpenPostgres(t)
	gymID := uuid.New()
	ownerID := uuid.New()

	exec := func(q string, args ...any) {
		t.Helper()
		if err := db.Exec(q, args...).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Bump Test Gym', 'MX', 'America/Mexico_City')`, gymID, gymID)
	exec(`INSERT INTO users (id, gym_id, version, created_at, updated_at, full_name, role, email)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Owner', 'owner', ?)`, ownerID, gymID, uuid.NewString()+"@t.mx")
	// Journal previo: un payment del primer import (version 3 — hubo
	// ediciones) y una fila de tipo conservado con versión alta (gyms:
	// TouchGym la bumpea seguido) que NO debe inflar la base del import.
	exec(`INSERT INTO sync_entities (gym_id, entity_type, entity_id, version, payload, server_updated_at)
	      VALUES (?, 'payments', ?, 3, '{}', NOW()), (?, 'gyms', ?, 50, '{}', NOW())`,
		gymID, uuid.New(), gymID, gymID)
	t.Cleanup(func() {
		for _, tbl := range []string{"sync_entities", "payments", "memberships", "membership_types",
			"members", "users", "gyms"} {
			_ = db.Exec("DELETE FROM "+tbl+" WHERE gym_id = ?", gymID).Error
		}
	})

	var gotVersion int
	if err := db.Transaction(func(tx *gorm.DB) error {
		v, err := nextImportVersion(tx, gymID)
		if err != nil {
			return err
		}
		gotVersion = v
		if err := wipeGym(tx, gymID); err != nil {
			return err
		}
		when := time.Now().Add(-24 * time.Hour)
		src := sourceData{
			memberships: []srcMembresia{{ID: 1, Nombre: "Mensual", Estado: 1, Precio: 550, Meses: 1}},
			socios:      []srcSocio{{ID: 1, Estado: 1, Nombre: "Rosa", Paterno: "Robles", Telefono: "4151112233", FechaCreacion: &when}},
			socioMembs: []srcSocioMembresia{{ID: 1, Estado: 1, IDSocio: 1, IDMembresia: 1, Precio: 550,
				FechaInicioMembresia: &when, Meses: 1, Vencimiento: &when}},
			socioMembsPagos: []srcSocioMembresiaPago{{ID: 1, Folio: 1, IDSocioMembresia: 1, Fecha: &when,
				Estado: 1, Importe: 550, IDTypePayment: 1}},
		}
		return importAll(tx, src, gymID, ownerID, gotVersion)
	}); err != nil {
		t.Fatalf("reimport: %v", err)
	}

	// Base = max(3 del payment) + headroom. La fila de gyms (50) se ignora.
	want := 3 + importVersionHeadroom
	if gotVersion != want {
		t.Fatalf("nextImportVersion = %d, want %d (max journal re-emitible 3 + headroom %d; si salió >50 está contando los tipos conservados)",
			gotVersion, want, importVersionHeadroom)
	}
	// Y esa versión aterriza idéntica en el journal y en el dominio: es la
	// que hace que el LWW del sidecar acepte el pull y pise la fila local.
	var journalVersion, domainVersion int
	if err := db.Raw(`SELECT version FROM sync_entities WHERE gym_id = ? AND entity_type = 'payments'`, gymID).
		Scan(&journalVersion).Error; err != nil {
		t.Fatalf("journal version: %v", err)
	}
	if err := db.Raw(`SELECT version FROM payments WHERE gym_id = ?`, gymID).
		Scan(&domainVersion).Error; err != nil {
		t.Fatalf("domain version: %v", err)
	}
	if journalVersion != want || domainVersion != want {
		t.Errorf("versions tras re-import: journal=%d domain=%d, want ambos %d", journalVersion, domainVersion, want)
	}
}

// Primer import de un gym sin historia: version 1, como siempre — el bump
// sólo entra cuando hay journal previo que superar.
func TestFirstImportKeepsVersionOne(t *testing.T) {
	db := testutil.OpenPostgres(t)
	gymID := uuid.New()

	if err := db.Exec(`INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Fresh Gym', 'MX', 'America/Mexico_City')`, gymID, gymID).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Journal sólo de tipos conservados — típico de un gym que ya operó
	// (toggles, subscription) pero nunca ha sido importado.
	if err := db.Exec(`INSERT INTO sync_entities (gym_id, entity_type, entity_id, version, payload, server_updated_at)
	      VALUES (?, 'gyms', ?, 50, '{}', NOW())`, gymID, gymID).Error; err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM sync_entities WHERE gym_id = ?", gymID).Error
		_ = db.Exec("DELETE FROM gyms WHERE gym_id = ?", gymID).Error
	})

	var got int
	if err := db.Transaction(func(tx *gorm.DB) error {
		v, err := nextImportVersion(tx, gymID)
		got = v
		return err
	}); err != nil {
		t.Fatalf("nextImportVersion: %v", err)
	}
	if got != 1 {
		t.Errorf("nextImportVersion en gym fresco = %d, want 1", got)
	}
}
