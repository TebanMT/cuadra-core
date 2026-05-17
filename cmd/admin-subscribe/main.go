//go:build server

// Package main — `cmd/admin-subscribe` registra una activación o renovación
// manual de suscripción para un gym. Pensado para el founder cuando un
// dueño paga por transferencia (sin pasar por Stripe).
//
// El use case `subscriptions/app/RecordEvent` ya soporta provider=manual:
// inserta el evento, marca el gym como `subscription_status='active'` con
// el plan + period_ends_at correctos, y deja audit log.
//
// Uso:
//
//	DATABASE_URL=postgresql://... \
//	    go run -tags server ./cmd/admin-subscribe \
//	        --gym-id <uuid>                    # gym destino
//	        --plan standard_monthly            # plan a activar
//	        --paid-until 2026-06-15            # cuándo expira este pago
//	        --amount 799                       # monto recibido en MXN (opcional)
//	        --note "Transferencia BBVA"        # texto libre en raw_payload (opcional)
//	        --renewal                          # marca el evento como renewed en vez de activated
//	        --dry-run                          # no escribe nada, solo valida
//
// Idempotencia: cada corrida usa un external_id determinístico por
// (gym, paid-until) — si re-corres con los mismos params, el RecordEvent
// detecta duplicado y no double-bills.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	infraDB "github.com/cuadra/cuadra-core/infraestructure/db"
	gymRepoPg "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	subApp "github.com/cuadra/cuadra-core/src/modules/subscriptions/app"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	subDB "github.com/cuadra/cuadra-core/src/modules/subscriptions/infraestructure/db"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

var validPlans = map[string]struct{}{
	"standard_monthly": {},
	"standard_annual":  {},
	"plus_monthly":     {},
	"plus_annual":      {},
}

func main() {
	var (
		gymIDFlag    = flag.String("gym-id", "", "destination gym UUID")
		planFlag     = flag.String("plan", "standard_monthly", "plan to activate (standard_monthly | standard_annual | plus_monthly | plus_annual)")
		paidUntilStr = flag.String("paid-until", "", "fecha cuándo expira este pago (YYYY-MM-DD)")
		amountFlag   = flag.Float64("amount", 0, "monto recibido en MXN (opcional, info para historial)")
		noteFlag     = flag.String("note", "", "texto libre para audit (ej: 'Transferencia BBVA folio 12345')")
		renewalFlag  = flag.Bool("renewal", false, "marca el evento como 'renewed' en vez de 'activated' (default: activated)")
		dryRun       = flag.Bool("dry-run", false, "valida sin escribir")
	)
	flag.Parse()

	if *gymIDFlag == "" {
		log.Fatalf("--gym-id es requerido")
	}
	gymID, err := uuid.Parse(*gymIDFlag)
	if err != nil {
		log.Fatalf("--gym-id inválido: %v", err)
	}
	plan := strings.TrimSpace(*planFlag)
	if _, ok := validPlans[plan]; !ok {
		log.Fatalf("--plan inválido: %s (válidos: standard_monthly, standard_annual, plus_monthly, plus_annual)", plan)
	}
	if *paidUntilStr == "" {
		log.Fatalf("--paid-until es requerido (YYYY-MM-DD)")
	}
	paidUntil, err := time.Parse("2006-01-02", *paidUntilStr)
	if err != nil {
		log.Fatalf("--paid-until inválido: %v", err)
	}
	if paidUntil.Before(time.Now().UTC().Truncate(24 * time.Hour)) {
		log.Fatalf("--paid-until no puede ser en el pasado")
	}

	// External ID determinístico — re-ejecutar con los mismos params es
	// idempotente. Si el founder se equivocó de fecha y re-corre con otra,
	// se inserta evento nuevo (intencional: dos pagos distintos).
	externalID := fmt.Sprintf("manual-%s-%s", gymID, paidUntil.Format("20060102"))

	rawPayload := map[string]any{
		"recorded_via": "cmd/admin-subscribe",
		"paid_until":   paidUntil.Format("2006-01-02"),
	}
	if *noteFlag != "" {
		rawPayload["note"] = *noteFlag
	}

	currency := "MXN"
	var amount *float64
	if *amountFlag > 0 {
		amount = amountFlag
	}

	eventType := subDomain.EventActivated
	if *renewalFlag {
		eventType = subDomain.EventRenewed
	}

	log.Printf("plan=%s gym=%s paid_until=%s external_id=%s type=%s amount=%v note=%q dry_run=%t",
		plan, gymID, paidUntil.Format("2006-01-02"), externalID, eventType, *amountFlag, *noteFlag, *dryRun)

	if *dryRun {
		log.Printf("dry-run: nada se escribió")
		return
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatalf("DATABASE_URL no está seteado")
	}
	db := infraDB.InitPostgres(dsn)
	defer infraDB.ClosePostgres()
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	recorder := audit.NewPostgresRecorder()
	gyms := gymRepoPg.NewGymPostgresRepository()
	events := subDB.NewEventPostgresRepository()

	uc := subApp.NewRecordEvent(events, gyms, uow, recorder)
	out, err := uc.Execute(context.Background(), subApp.RecordEventInput{
		GymID:        gymID,
		Provider:     subDomain.ProviderManual,
		Type:         eventType,
		ExternalID:   externalID,
		Plan:         plan,
		Amount:       amount,
		Currency:     &currency,
		PeriodEndsAt: &paidUntil,
		RawPayload:   rawPayload,
		OccurredAt:   time.Now().UTC(),
	})
	if err != nil {
		log.Fatalf("RecordEvent: %v", err)
	}
	if !out.Applied {
		log.Printf("idempotente: ya existía un evento con external_id=%s, no se aplicó cambio", externalID)
		return
	}
	log.Printf("ok: gym=%s ahora en plan=%s status=active period_ends_at=%s event_id=%s",
		gymID, plan, paidUntil.Format("2006-01-02"), out.EventID)
}
