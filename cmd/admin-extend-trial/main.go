//go:build server

// Package main — `cmd/admin-extend-trial` extiende la prueba gratuita de un
// gym. Es una herramienta del founder (pilotos, ventas): antes era un
// endpoint que cualquier dueño podía llamar con su sesión para saltarse el
// fin de la prueba (UC-002). Ahora solo se corre en el servidor, con acceso
// a la base.
//
// Usa el mismo RecordEvent que los webhooks: guarda un evento trial_extended
// en subscription_events, mueve trial_ends_at, deja audit log y avisa a las
// recepciones por sync. Si el cloud ya había cerrado la prueba (gracia o
// solo lectura), la reabre; cuando venza la nueva fecha, el job de pruebas
// vencidas la vuelve a cerrar.
//
// Uso:
//
//	DATABASE_URL=postgresql://... \
//	    go run -tags server ./cmd/admin-extend-trial \
//	        --gym-id <uuid>          # gym destino (debe seguir en prueba)
//	        --days 14                # días a sumar, 1 a 90
//	        --note "Piloto Gym Ejemplo"  # texto libre para el historial (opcional)
//	        --dry-run                # muestra la fecha nueva sin escribir
//
// Los días se suman a partir de hoy o del fin actual de la prueba, lo que
// sea más tarde. Re-correr el mismo comando el mismo día no extiende dos
// veces.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"

	infraDB "github.com/cuadra/cuadra-core/infraestructure/db"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	gymRepoPg "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	subApp "github.com/cuadra/cuadra-core/src/modules/subscriptions/app"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	subDB "github.com/cuadra/cuadra-core/src/modules/subscriptions/infraestructure/db"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

const maxExtensionDays = 90

type extendInput struct {
	GymID  uuid.UUID
	Days   int
	Note   string
	DryRun bool
}

type extendResult struct {
	PreviousEnd *time.Time
	NewEnd      time.Time
	Applied     bool
	EventID     uuid.UUID
}

var (
	errNotOnTrial = errors.New("el gym ya no está en prueba; la extensión es solo para pruebas")
	errGymMissing = errors.New("no existe un gym con ese id")
)

func main() {
	var (
		gymIDFlag = flag.String("gym-id", "", "gym destino (UUID)")
		daysFlag  = flag.Int("days", 0, fmt.Sprintf("días a sumar a la prueba (1 a %d)", maxExtensionDays))
		noteFlag  = flag.String("note", "", "texto libre para el historial (ej: 'Piloto Gym Ejemplo')")
		dryRun    = flag.Bool("dry-run", false, "muestra la fecha nueva sin escribir")
	)
	flag.Parse()

	gymID, err := uuid.Parse(*gymIDFlag)
	if err != nil {
		log.Fatalf("--gym-id inválido o faltante: %v", err)
	}
	if err := validateDays(*daysFlag); err != nil {
		log.Fatal(err)
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatalf("DATABASE_URL no está seteado")
	}
	db := infraDB.InitPostgres(dsn)
	defer infraDB.ClosePostgres()
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	gyms := gymRepoPg.NewGymPostgresRepository()
	recorder := subApp.NewRecordEvent(subDB.NewEventPostgresRepository(), gyms, uow, audit.NewPostgresRecorder())
	recorder.SyncTouch = syncpkg.NewPostgresStore()
	// Un solo reloj: la fecha que se imprime es la que se guarda.
	now := time.Now().UTC()
	recorder.NowFunc = func() time.Time { return now }

	in := extendInput{GymID: gymID, Days: *daysFlag, Note: *noteFlag, DryRun: *dryRun}
	out, err := extendTrial(context.Background(), uow, gyms, recorder, in, now)
	if err != nil {
		log.Fatal(err)
	}
	previous := "sin fecha"
	if out.PreviousEnd != nil {
		previous = out.PreviousEnd.Format(time.RFC3339)
	}
	switch {
	case in.DryRun:
		log.Printf("dry-run: gym=%s prueba terminaba=%s, terminaría=%s. Nada se escribió.", gymID, previous, out.NewEnd.Format(time.RFC3339))
	case !out.Applied:
		log.Printf("ya aplicado hoy: gym=%s no se extendió otra vez (prueba termina=%s)", gymID, previous)
	default:
		log.Printf("ok: gym=%s prueba terminaba=%s, ahora termina=%s event_id=%s", gymID, previous, out.NewEnd.Format(time.RFC3339), out.EventID)
	}
}

func validateDays(days int) error {
	if days < 1 || days > maxExtensionDays {
		return fmt.Errorf("--days debe ser de 1 a %d", maxExtensionDays)
	}
	return nil
}

// trialExtendedExternalID es la llave de idempotencia: el mismo gym, los
// mismos días y el mismo día UTC son una sola extensión.
func trialExtendedExternalID(gymID uuid.UUID, days int, now time.Time) string {
	return fmt.Sprintf("trial-extended:%s:%dd:%s", gymID, days, now.UTC().Format("2006-01-02"))
}

func extendTrial(ctx context.Context, uow sharedDomain.UnitOfWork, gyms gymRepo.GymRepository, recorder *subApp.RecordEvent, in extendInput, now time.Time) (extendResult, error) {
	if err := validateDays(in.Days); err != nil {
		return extendResult{}, err
	}
	ctx = sharedDomain.WithTenant(ctx, in.GymID)
	tx, err := uow.Query(ctx)
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		return extendResult{}, err
	}
	g, err := gyms.GetByID(tx, in.GymID)
	sharedDomain.CloseQuery(tx)
	if err != nil || g == nil {
		return extendResult{}, errGymMissing
	}
	if g.SubscriptionPlan != gymDomain.PlanTrial {
		return extendResult{}, fmt.Errorf("%w (plan actual: %s)", errNotOnTrial, g.SubscriptionPlan)
	}
	// Misma cuenta que aplicará RecordEvent, sobre una copia.
	preview := *g
	preview.ExtendTrial(in.Days, now)
	out := extendResult{PreviousEnd: g.TrialEndsAt, NewEnd: *preview.TrialEndsAt}
	if in.DryRun {
		return out, nil
	}
	payload := map[string]any{"days": float64(in.Days), "recorded_via": "cmd/admin-extend-trial"}
	if in.Note != "" {
		payload["note"] = in.Note
	}
	rec, err := recorder.Execute(ctx, subApp.RecordEventInput{
		GymID:      in.GymID,
		Provider:   subDomain.ProviderManual,
		Type:       subDomain.EventTrialExtended,
		ExternalID: trialExtendedExternalID(in.GymID, in.Days, now),
		RawPayload: payload,
		OccurredAt: now,
	})
	if err != nil {
		return extendResult{}, err
	}
	out.Applied, out.EventID = rec.Applied, rec.EventID
	return out, nil
}
