package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Estos tests pinean el contrato tz-aware de UC-038 (dogfood 6-jul-2026):
//   - las etapas se evalúan con el día LOCAL de cada gym (delegado al
//     reader; aquí verificamos que se pida por offset con el now crudo).
//   - el envío se agenda dentro de la ventana 8AM–9PM local del gym vía
//     scheduled_for — el tick puede correr a cualquier hora sin que a
//     ningún socio le llegue un WhatsApp de madrugada.
//   - idempotencia por (member, offset, expiry) — el mismo formato de
//     llave de siempre.

// fakeExpiryReader devuelve candidatos fijos por offset y registra qué
// offsets pidió el tick. errByOffset simula una etapa rota.
type fakeExpiryReader struct {
	byOffset     map[int][]ExpiryCandidate
	errByOffset  map[int]error
	offsetsAsked []int
}

func (f *fakeExpiryReader) FindDueForStage(_ sharedDomain.Transaction, _ time.Time, offsetDays int) ([]ExpiryCandidate, error) {
	f.offsetsAsked = append(f.offsetsAsked, offsetDays)
	if err := f.errByOffset[offsetDays]; err != nil {
		return nil, err
	}
	return f.byOffset[offsetDays], nil
}

func expiryFixture(byOffset map[int][]ExpiryCandidate) (*EnqueueExpiryReminder, *fakeNotiRepo, *fakeExpiryReader) {
	notis := &fakeNotiRepo{byKey: map[string]*notiDomain.Notification{}}
	reader := &fakeExpiryReader{byOffset: byOffset}
	uc := NewEnqueueExpiryReminder(notis, reader, fakeUoW{})
	return uc, notis, reader
}

func candidate(tzName string, expiry time.Time) ExpiryCandidate {
	return ExpiryCandidate{
		GymID:          uuid.New(),
		GymName:        "Gym Pilot",
		GymTimezone:    tzName,
		MemberID:       uuid.New(),
		MemberFullName: "Rosa Robles",
		MemberPhone:    "5522334455",
		MembershipType: "Mensual",
		ExpiryDate:     expiry,
	}
}

func TestTick_PideLasTresEtapasYEncola(t *testing.T) {
	expiry := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	uc, notis, reader := expiryFixture(map[int][]ExpiryCandidate{
		-3: {candidate("America/Mexico_City", expiry)},
	})

	// 6-jul 10:00 AM CDMX (16:00 UTC) — dentro de la ventana de envío.
	now := time.Date(2026, 7, 6, 16, 0, 0, 0, time.UTC)
	n, err := uc.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 1 || len(notis.created) != 1 {
		t.Fatalf("inserted=%d created=%d, want 1/1", n, len(notis.created))
	}
	// Las tres etapas de DA-38.1, con el now crudo (la cuenta local la
	// hace el reader por gym).
	wantOffsets := []int{-3, 0, 5}
	for i, o := range wantOffsets {
		if reader.offsetsAsked[i] != o {
			t.Errorf("offset[%d] = %d, want %d", i, reader.offsetsAsked[i], o)
		}
	}
	row := notis.created[0]
	if row.TemplateKey != "expiry_reminder_3d" {
		t.Errorf("template = %q", row.TemplateKey)
	}
	wantKey := "expiry_reminder:" + row.RecipientID.String() + ":-3:2026-07-09"
	if row.IdempotencyKey == nil || *row.IdempotencyKey != wantKey {
		t.Errorf("idempotency = %v, want %s", row.IdempotencyKey, wantKey)
	}
	// Dentro de la ventana → sale ya (scheduled_for = now).
	if !row.ScheduledFor.Equal(now) {
		t.Errorf("scheduled_for = %v, want now %v", row.ScheduledFor, now)
	}
}

func TestTick_MadrugadaLocalAgendaALas8AM(t *testing.T) {
	expiry := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	uc, notis, _ := expiryFixture(map[int][]ExpiryCandidate{
		-3: {candidate("America/Mexico_City", expiry)},
	})

	// 6-jul 01:00 AM CDMX (07:00 UTC) — el tick corre pero el mensaje NO
	// debe salir de madrugada.
	now := time.Date(2026, 7, 6, 7, 0, 0, 0, time.UTC)
	if _, err := uc.Tick(context.Background(), now); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// 8:00 AM CDMX = 14:00 UTC del mismo día local.
	want := time.Date(2026, 7, 6, 14, 0, 0, 0, time.UTC)
	if got := notis.created[0].ScheduledFor; !got.Equal(want) {
		t.Errorf("scheduled_for = %v, want 8AM local %v", got, want)
	}
}

func TestTick_NocheLocalAgendaAManana8AM(t *testing.T) {
	expiry := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	uc, notis, _ := expiryFixture(map[int][]ExpiryCandidate{
		-3: {candidate("America/Mexico_City", expiry)},
	})

	// 6-jul 10:00 PM CDMX (7-jul 04:00 UTC) — pasada la ventana: mañana.
	now := time.Date(2026, 7, 7, 4, 0, 0, 0, time.UTC)
	if _, err := uc.Tick(context.Background(), now); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// 7-jul 8:00 AM CDMX = 7-jul 14:00 UTC.
	want := time.Date(2026, 7, 7, 14, 0, 0, 0, time.UTC)
	if got := notis.created[0].ScheduledFor; !got.Equal(want) {
		t.Errorf("scheduled_for = %v, want mañana 8AM local %v", got, want)
	}
}

func TestTick_IdempotenciaPorMiembroEtapaYFecha(t *testing.T) {
	expiry := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	c := candidate("America/Mexico_City", expiry)
	uc, notis, _ := expiryFixture(map[int][]ExpiryCandidate{-3: {c}})

	now := time.Date(2026, 7, 6, 16, 0, 0, 0, time.UTC)
	if _, err := uc.Tick(context.Background(), now); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	// Simular que la fila ya existe (el fake no indexa por llave al crear).
	created := notis.created[0]
	notis.byKey[*created.IdempotencyKey] = created

	n, err := uc.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if n != 0 || len(notis.created) != 1 {
		t.Errorf("segundo tick insertó %d (created=%d), want 0/1", n, len(notis.created))
	}
}

// Lección del incidente 42883 (jul-2026): un error determinista en la
// etapa -3 abortaba el tick entero y callaba también a "vence hoy" y a la
// persecución +5, en CADA tick, durante días. Una etapa rota no debe
// silenciar a las demás.
func TestTick_UnaEtapaRotaNoCallaALasDemas(t *testing.T) {
	expiry := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	uc, notis, reader := expiryFixture(map[int][]ExpiryCandidate{
		0: {candidate("America/Mexico_City", expiry)},
	})
	reader.errByOffset = map[int]error{-3: errors.New("boom 42883")}

	now := time.Date(2026, 7, 6, 16, 0, 0, 0, time.UTC)
	n, err := uc.Tick(context.Background(), now)

	// El error de la etapa -3 se REPORTA (el Scheduler lo loguea)…
	if err == nil || !strings.Contains(err.Error(), "stage -3") {
		t.Errorf("err = %v, want error de stage -3", err)
	}
	// …pero las etapas 0 y +5 corrieron igual, y la 0 encoló su fila.
	wantOffsets := []int{-3, 0, 5}
	if len(reader.offsetsAsked) != len(wantOffsets) {
		t.Fatalf("offsets pedidos = %v, want %v", reader.offsetsAsked, wantOffsets)
	}
	if n != 1 || len(notis.created) != 1 {
		t.Errorf("inserted=%d created=%d, want 1/1 (la etapa 0 no debió callarse)", n, len(notis.created))
	}
}

func TestTick_SinTelefonoSeSalta(t *testing.T) {
	expiry := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	c := candidate("America/Mexico_City", expiry)
	c.MemberPhone = "  "
	uc, notis, _ := expiryFixture(map[int][]ExpiryCandidate{-3: {c}})

	n, err := uc.Tick(context.Background(), time.Date(2026, 7, 6, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 0 || len(notis.created) != 0 {
		t.Errorf("sin teléfono no debe encolar, got %d", len(notis.created))
	}
}

func TestScheduleWithinSendWindow_TzInvalidaCaeAUTC(t *testing.T) {
	// Gym sin tz: la ventana se evalúa en UTC — mejor una hora rara que
	// reventar el tick de todos los gyms.
	now := time.Date(2026, 7, 6, 22, 30, 0, 0, time.UTC)
	got := scheduleWithinSendWindow("", now)
	want := time.Date(2026, 7, 7, 8, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("scheduled = %v, want %v", got, want)
	}
}
