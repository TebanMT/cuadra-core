//go:build sidecar

package app_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notiRepoLite "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
)

// El TEXTO de las plantillas sólo se personaliza en Plus: en Standard lo que
// sale por WhatsApp es el Twilio Content SID aprobado por Meta, así que un body
// editado no cambiaría el mensaje. El use case fuerza el body al default sin
// Plus, pero respeta el switch on/off.
func TestUpdateTemplate_StandardCoercesBodyToDefault_TogglesStillApply(t *testing.T) {
	f := newFixture(t)
	recorder := audit.NewSQLiteRecorder()
	templateRepo := notiRepoLite.NewTemplateOverrideSQLiteRepository()

	// Tomamos una plantilla real + su default desde la vista (sorted by key).
	listUC := notiApp.NewListTemplates(templateRepo, f.uow)
	views, err := listUC.Execute(context.Background(), f.gymID)
	if err != nil || len(views) == 0 {
		t.Fatalf("list templates: %v (n=%d)", err, len(views))
	}
	key := views[0].Key
	defaultBody := views[0].Default

	updateUC := notiApp.NewUpdateTemplate(templateRepo, f.uow, recorder)

	// Standard (CanEditBody=false): el body se fuerza al default aprobado por
	// Meta; el switch on/off SÍ se aplica.
	updateUC.CanEditBody = func(context.Context, uuid.UUID) bool { return false }
	off := false
	saved, err := updateUC.Execute(context.Background(), notiApp.UpdateTemplateInput{
		GymID:       f.gymID,
		ActorUserID: f.ownerID,
		Key:         key,
		Body:        "TEXTO PIRATA que Meta no aprobo",
		Enabled:     &off,
	})
	if err != nil {
		t.Fatalf("update (standard): %v", err)
	}
	if saved.Body != defaultBody {
		t.Errorf("Standard debe forzar el body al default; got %q want %q", saved.Body, defaultBody)
	}
	if saved.Enabled {
		t.Errorf("el switch on/off SÍ debe respetarse en Standard (enabled=false)")
	}

	// Plus (CanEditBody=true): el texto personalizado SÍ persiste. Partimos del
	// default y le añadimos un literal para no introducir variables inválidas.
	updateUC.CanEditBody = func(context.Context, uuid.UUID) bool { return true }
	custom := defaultBody + " — personalizado por el gym"
	saved2, err := updateUC.Execute(context.Background(), notiApp.UpdateTemplateInput{
		GymID:       f.gymID,
		ActorUserID: f.ownerID,
		Key:         key,
		Body:        custom,
	})
	if err != nil {
		t.Fatalf("update (plus): %v", err)
	}
	if saved2.Body != custom {
		t.Errorf("Plus debe permitir personalizar el texto; got %q want %q", saved2.Body, custom)
	}

	// Sin gate cableado (CanEditBody nil) = fail-open: permite editar (tests /
	// binarios que no montan el dominio de gyms).
	updateUC.CanEditBody = nil
	custom2 := defaultBody + " — fail open"
	saved3, err := updateUC.Execute(context.Background(), notiApp.UpdateTemplateInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Key: key, Body: custom2,
	})
	if err != nil {
		t.Fatalf("update (nil gate): %v", err)
	}
	if saved3.Body != custom2 {
		t.Errorf("CanEditBody nil debe ser fail-open (permite); got %q", saved3.Body)
	}
}
