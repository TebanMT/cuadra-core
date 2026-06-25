//go:build sidecar

package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
)

// El payload de sync de members antes omitía birthdate, photo_url, notes,
// last_maintenance_paid, last_contact_attempt_at y deleted_at → nunca
// llegaban al cloud (reporte de cumpleaños, persecución por pago y el
// soft-delete del socio quedaban rotos). Este test fija que ahora viajan.
func TestSync_EnqueueMember_CarriesPreviouslyDroppedFields(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	bday := time.Date(1990, 5, 15, 0, 0, 0, 0, time.UTC)
	uc := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	if _, err := uc.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Juan Pérez", Phone: "+524421234567",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
		Birthdate: &bday,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	var payload string
	if err := f.db.Get(&payload,
		`SELECT payload FROM sync_queue WHERE entity_type='members' ORDER BY enqueued_at DESC LIMIT 1`); err != nil {
		t.Fatalf("read sync_queue: %v", err)
	}
	for _, key := range []string{
		"birthdate", "photo_url", "notes",
		"last_maintenance_paid", "last_contact_attempt_at", "deleted_at",
	} {
		if !strings.Contains(payload, `"`+key+`"`) {
			t.Errorf("el payload de sync de members no incluye %q\npayload: %s", key, payload)
		}
	}
	// Formato de fecha ISO (igual que memberToRow) para que round-trippee.
	if !strings.Contains(payload, `"birthdate":"1990-05-15"`) {
		t.Errorf("birthdate mal formateado en el payload: %s", payload)
	}
}
