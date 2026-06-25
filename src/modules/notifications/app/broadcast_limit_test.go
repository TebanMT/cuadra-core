//go:build sidecar

package app_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Regresión: el repo de socios recorta page_size>200 a 25. El broadcast debe
// PAGINAR y traer toda la audiencia, no quedarse en 25 (bug silencioso: los
// envíos sólo llegaban a 25 socios).
func TestBroadcast_AudiencePaginatesBeyondPageClamp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for i := 0; i < 26; i++ {
		mid := uuid.New()
		m, err := memberDomain.NewMember(
			mid, f.gymID, fmt.Sprintf("p%04d", 2000+i),
			fmt.Sprintf("Socio %02d", i), fmt.Sprintf("44430000%02d", i),
			f.ownerID, now,
		)
		if err != nil {
			t.Fatalf("new member %d: %v", i, err)
		}
		if err := f.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
			_, e := f.memberRepo.Create(tx, m)
			return e
		}); err != nil {
			t.Fatalf("create member %d: %v", i, err)
		}
		if _, err := f.registerUC.Execute(ctx, billingApp.RegisterMembershipPaymentInput{
			GymID: f.gymID, ActorUserID: f.ownerID,
			MemberID: mid, MembershipTypeID: f.planID, Method: "cash",
		}); err != nil {
			t.Fatalf("activate member %d: %v", i, err)
		}
	}

	bc := notiApp.NewBroadcast(
		f.notiRepo, f.memberRepo, f.gymRepo,
		audit.NewSQLiteReader(), f.uow, audit.NewSQLiteRecorder(),
	)
	pre, err := bc.Execute(ctx, notiApp.BroadcastInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Filter: notiApp.BroadcastFilterAllActive, Confirmed: false,
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if pre.AudienceN < 26 || len(pre.AudiencePreview) < 26 {
		t.Errorf("paginación: esperaba >=26 socios, got n=%d preview=%d (¿se recortó a 25?)",
			pre.AudienceN, len(pre.AudiencePreview))
	}
}

// Opción C: selección manual de socios por IDs. Gana sobre el filtro; el
// preview devuelve la lista (id+nombre) para sembrar el FE; anti cross-tenant.
func TestBroadcast_MemberSelection(t *testing.T) {
	f := newFixture(t)
	bc := notiApp.NewBroadcast(
		f.notiRepo, f.memberRepo, f.gymRepo,
		audit.NewSQLiteReader(), f.uow, audit.NewSQLiteRecorder(),
	)

	// Preview por IDs: devuelve la lista, sin importar el status del socio.
	pre, err := bc.Execute(context.Background(), notiApp.BroadcastInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberIDs: []uuid.UUID{f.memberID}, Confirmed: false,
	})
	if err != nil {
		t.Fatalf("preview manual: %v", err)
	}
	if pre.AudienceN != 1 || len(pre.AudiencePreview) != 1 || pre.AudiencePreview[0].ID != f.memberID {
		t.Errorf("preview manual mal: n=%d preview=%+v", pre.AudienceN, pre.AudiencePreview)
	}

	// Confirmar envía a la selección.
	out, err := bc.Execute(context.Background(), notiApp.BroadcastInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberIDs: []uuid.UUID{f.memberID}, Message: "Hola, te esperamos", Confirmed: true,
	})
	if err != nil || out.EnqueuedN != 1 {
		t.Fatalf("envío manual: err=%v out=%+v", err, out)
	}

	// Anti cross-tenant: un ID inexistente / de otro gym no resuelve.
	pre2, err := bc.Execute(context.Background(), notiApp.BroadcastInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberIDs: []uuid.UUID{uuid.New()}, Confirmed: false,
	})
	if err != nil {
		t.Fatalf("preview cross: %v", err)
	}
	if pre2.AudienceN != 0 {
		t.Errorf("ID ajeno no debe resolver, got %d", pre2.AudienceN)
	}
}

// 2C: el envío masivo entra en Standard pero acotado a 2 envíos/mes y 100
// socios/envío. El tercer envío del mes se bloquea.
func TestBroadcast_StandardMonthlyLimit(t *testing.T) {
	f := newFixture(t)

	// Activa al socio (con teléfono) para que all_active lo incluya.
	if _, err := f.registerUC.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "cash",
	}); err != nil {
		t.Fatalf("activar socio: %v", err)
	}

	bc := notiApp.NewBroadcast(
		f.notiRepo, f.memberRepo, f.gymRepo,
		audit.NewSQLiteReader(), f.uow, audit.NewSQLiteRecorder(),
	)

	// Preview: límites Standard visibles, sin requerir mensaje ni abortar.
	pre, err := bc.Execute(context.Background(), notiApp.BroadcastInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Filter: notiApp.BroadcastFilterAllActive, Confirmed: false,
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if pre.IsPlus || pre.MonthlyLimit != 2 || pre.AudienceCap != 150 {
		t.Errorf("límites Standard mal: %+v", pre)
	}
	if pre.AudienceN != 1 {
		t.Errorf("audiencia esperada 1, got %d", pre.AudienceN)
	}
	if pre.MonthlyUsed != 0 {
		t.Errorf("monthly_used esperado 0 al inicio, got %d", pre.MonthlyUsed)
	}

	send := func() (*notiApp.BroadcastOutput, error) {
		return bc.Execute(context.Background(), notiApp.BroadcastInput{
			GymID: f.gymID, ActorUserID: f.ownerID,
			Filter:    notiApp.BroadcastFilterAllActive,
			Message:   "Hola, te esperamos en el gym",
			Confirmed: true,
		})
	}

	if out, err := send(); err != nil || out.EnqueuedN != 1 {
		t.Fatalf("envío 1: err=%v enqueued=%v", err, out)
	}
	if _, err := send(); err != nil {
		t.Fatalf("envío 2: %v", err)
	}
	// Tercer envío del mes → bloqueado por el tope mensual.
	_, err = send()
	if err == nil || !strings.Contains(err.Error(), notiErrors.ErrBroadcastMonthlyLimit.Error()) {
		t.Errorf("envío 3 debe dar ErrBroadcastMonthlyLimit, got %v", err)
	}
}
