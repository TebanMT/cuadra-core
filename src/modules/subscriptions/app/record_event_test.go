package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Tests for RecordEvent — the subscriptions webhook write path. Covers the
// adversarial cases the v0.6 spec gate calls out: idempotent replay,
// out-of-order delivery, missing plan on activated, the trial→past_due
// →cancelled lifecycle. No DB — fakes only, so this stays unit-fast.

// ── Fakes ────────────────────────────────────────────────────────────────

type fakeTx struct{}

func (fakeTx) Execute(fn func(tx sharedDomain.Transaction) error) error { return fn(fakeTx{}) }

type fakeUoW struct{}

func (fakeUoW) Begin(_ context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Commit(_ sharedDomain.Transaction) error                   { return nil }
func (fakeUoW) Rollback(_ sharedDomain.Transaction) error                 { return nil }
func (fakeUoW) Query(_ context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Command(ctx context.Context, fn func(tx sharedDomain.Transaction) error) error {
	return fn(fakeTx{})
}

type fakeAudit struct{ entries []audit.Entry }

func (a *fakeAudit) Record(_ context.Context, _ sharedDomain.Transaction, e audit.Entry) error {
	a.entries = append(a.entries, e)
	return nil
}

type fakeEvents struct {
	rows []*subDomain.Event
}

func (r *fakeEvents) Insert(_ sharedDomain.Transaction, e *subDomain.Event) error {
	for _, existing := range r.rows {
		if existing.Provider == e.Provider && existing.ExternalID == e.ExternalID {
			return subDomain.ErrDuplicateEvent
		}
	}
	r.rows = append(r.rows, e)
	return nil
}

func (r *fakeEvents) ListByGym(_ sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]*subDomain.Event, error) {
	out := []*subDomain.Event{}
	for _, e := range r.rows {
		if e.GymID == gymID {
			out = append(out, e)
		}
	}
	if len(out) > limit && limit > 0 {
		out = out[:limit]
	}
	return out, nil
}

func (r *fakeEvents) ExistsByExternalID(_ sharedDomain.Transaction, p subDomain.Provider, id string) (bool, error) {
	for _, e := range r.rows {
		if e.Provider == p && e.ExternalID == id {
			return true, nil
		}
	}
	return false, nil
}

func (r *fakeEvents) LatestOccurredAtForGym(_ sharedDomain.Transaction, gymID uuid.UUID) (*time.Time, error) {
	var max time.Time
	for _, e := range r.rows {
		if e.GymID == gymID && e.OccurredAt.After(max) {
			max = e.OccurredAt
		}
	}
	if max.IsZero() {
		return nil, nil
	}
	return &max, nil
}

type fakeGyms struct {
	byID map[uuid.UUID]*gymDomain.Gym
}

func (r *fakeGyms) Create(_ sharedDomain.Transaction, g *gymDomain.Gym) (*gymDomain.Gym, error) {
	if r.byID == nil {
		r.byID = map[uuid.UUID]*gymDomain.Gym{}
	}
	r.byID[g.ID] = g
	return g, nil
}

func (r *fakeGyms) GetByID(_ sharedDomain.Transaction, id uuid.UUID) (*gymDomain.Gym, error) {
	if g, ok := r.byID[id]; ok {
		return g, nil
	}
	return nil, errors.New("gym not found")
}

func (r *fakeGyms) Update(_ sharedDomain.Transaction, g *gymDomain.Gym) (*gymDomain.Gym, error) {
	r.byID[g.ID] = g
	return g, nil
}

func (r *fakeGyms) HasMembershipType(_ sharedDomain.Transaction, _ uuid.UUID) (bool, error) {
	return false, nil
}

func (r *fakeGyms) ExistsByWhatsApp(_ sharedDomain.Transaction, _ string, _ uuid.UUID) (bool, error) {
	return false, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────

func newTrialGym(t *testing.T) (*fakeGyms, uuid.UUID) {
	t.Helper()
	g := &gymDomain.Gym{
		ID:                 uuid.New(),
		SubscriptionPlan:   gymDomain.PlanTrial,
		SubscriptionStatus: gymDomain.StatusActive,
	}
	r := &fakeGyms{byID: map[uuid.UUID]*gymDomain.Gym{g.ID: g}}
	return r, g.ID
}

func newRecordEvent(t *testing.T, gyms *fakeGyms, events *fakeEvents, now time.Time) *RecordEvent {
	t.Helper()
	return &RecordEvent{
		Events:  events,
		Gyms:    gyms,
		UoW:     fakeUoW{},
		Audit:   &fakeAudit{},
		NowFunc: func() time.Time { return now },
	}
}

// ── Tests ────────────────────────────────────────────────────────────────

func TestRecordEvent_Activate(t *testing.T) {
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	uc := newRecordEvent(t, gyms, events, now)

	out, err := uc.Execute(context.Background(), RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderStripe,
		Type:       subDomain.EventActivated,
		ExternalID: "evt_001",
		Plan:       gymDomain.PlanStandardMonthly,
		OccurredAt: now,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Applied {
		t.Fatalf("expected Applied=true")
	}
	g := gyms.byID[gymID]
	if g.SubscriptionPlan != gymDomain.PlanStandardMonthly {
		t.Fatalf("plan = %q, want %q", g.SubscriptionPlan, gymDomain.PlanStandardMonthly)
	}
	if g.SubscriptionStatus != gymDomain.StatusActive {
		t.Fatalf("status = %q, want active", g.SubscriptionStatus)
	}
}

func TestRecordEvent_IdempotentReplay(t *testing.T) {
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	uc := newRecordEvent(t, gyms, events, now)

	in := RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderStripe,
		Type:       subDomain.EventActivated,
		ExternalID: "evt_dup",
		Plan:       gymDomain.PlanStandardMonthly,
		OccurredAt: now,
	}
	// First delivery: applied.
	first, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("first call err: %v", err)
	}
	if !first.Applied {
		t.Fatalf("first call should apply")
	}
	// Second delivery (Stripe retry): idempotent no-op.
	second, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if second.Applied {
		t.Fatalf("second call should be idempotent (Applied=false)")
	}
	if len(events.rows) != 1 {
		t.Fatalf("expected 1 event row, got %d", len(events.rows))
	}
}

func TestRecordEvent_RejectsOutOfOrder(t *testing.T) {
	// Scenario: cancelled at T2 arrives and is applied. A delayed retry of a
	// renewal at T1 arrives later. The retry must NOT regress the gym to
	// active — RecordEvent rejects strictly-older events even when their
	// external_id is new.
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	t1 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(1 * time.Hour)
	uc := newRecordEvent(t, gyms, events, t2)

	// First activate at t1 (so the gym is in a paid plan).
	if _, err := uc.Execute(context.Background(), RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderStripe,
		Type:       subDomain.EventActivated,
		ExternalID: "evt_act",
		Plan:       gymDomain.PlanStandardMonthly,
		OccurredAt: t1,
	}); err != nil {
		t.Fatalf("activate err: %v", err)
	}
	// Cancel at t2.
	if _, err := uc.Execute(context.Background(), RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderStripe,
		Type:       subDomain.EventCancelled,
		ExternalID: "evt_can",
		OccurredAt: t2,
	}); err != nil {
		t.Fatalf("cancel err: %v", err)
	}
	if gyms.byID[gymID].SubscriptionStatus != gymDomain.StatusCancelled {
		t.Fatalf("expected cancelled after t2")
	}
	// Now a retry of an older renewal arrives — should NOT regress to active.
	out, err := uc.Execute(context.Background(), RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderStripe,
		Type:       subDomain.EventRenewed,
		ExternalID: "evt_old_renew",
		Plan:       gymDomain.PlanStandardMonthly,
		OccurredAt: t1.Add(-1 * time.Minute), // strictly older than the existing latest
	})
	if err != nil {
		t.Fatalf("late retry err: %v", err)
	}
	if out.Applied {
		t.Fatalf("late retry should be dropped (Applied=false)")
	}
	if gyms.byID[gymID].SubscriptionStatus != gymDomain.StatusCancelled {
		t.Fatalf("status regressed to %q — out-of-order retry corrupted state",
			gyms.byID[gymID].SubscriptionStatus)
	}
}

func TestRecordEvent_TrialToPastDueToCancelled(t *testing.T) {
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	uc := newRecordEvent(t, gyms, events, t0)

	// Step 1: trial → active (first cobro succeeds).
	if _, err := uc.Execute(context.Background(), RecordEventInput{
		GymID: gymID, Provider: subDomain.ProviderStripe,
		Type: subDomain.EventActivated, ExternalID: "evt_1",
		Plan: gymDomain.PlanStandardMonthly, OccurredAt: t0,
	}); err != nil {
		t.Fatalf("step1 err: %v", err)
	}
	if gyms.byID[gymID].SubscriptionStatus != gymDomain.StatusActive ||
		gyms.byID[gymID].SubscriptionPlan != gymDomain.PlanStandardMonthly {
		t.Fatalf("after activate: %+v", gyms.byID[gymID])
	}
	// Step 2: cobro mensual falla → past_due.
	if _, err := uc.Execute(context.Background(), RecordEventInput{
		GymID: gymID, Provider: subDomain.ProviderStripe,
		Type: subDomain.EventPastDue, ExternalID: "evt_2",
		OccurredAt: t0.Add(time.Hour),
	}); err != nil {
		t.Fatalf("step2 err: %v", err)
	}
	if gyms.byID[gymID].SubscriptionStatus != gymDomain.StatusPastDue {
		t.Fatalf("after past_due: %+v", gyms.byID[gymID])
	}
	// Plan should NOT change while past_due — the dueño can still recover by
	// updating their card. Cuadra-spec §9.4: "modo solo lectura ... bloquea
	// altas y cobros nuevos hasta regularizar."
	if gyms.byID[gymID].SubscriptionPlan != gymDomain.PlanStandardMonthly {
		t.Fatalf("plan changed during past_due — should stay %q",
			gymDomain.PlanStandardMonthly)
	}
	// Step 3: tres fallos consecutivos → cancelled.
	if _, err := uc.Execute(context.Background(), RecordEventInput{
		GymID: gymID, Provider: subDomain.ProviderStripe,
		Type: subDomain.EventCancelled, ExternalID: "evt_3",
		OccurredAt: t0.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("step3 err: %v", err)
	}
	if gyms.byID[gymID].SubscriptionStatus != gymDomain.StatusCancelled {
		t.Fatalf("after cancel: %+v", gyms.byID[gymID])
	}
}

func TestRecordEvent_ActivateRequiresPaidPlan(t *testing.T) {
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	uc := newRecordEvent(t, gyms, events, now)

	// Empty plan + gym sigue en trial → debe rechazar (no podemos activar a
	// trial; un evento de Stripe sin plan-hint cuando el gym sigue en trial
	// es señal de pricing mal mapeado).
	_, err := uc.Execute(context.Background(), RecordEventInput{
		GymID: gymID, Provider: subDomain.ProviderStripe,
		Type: subDomain.EventActivated, ExternalID: "evt_noplan",
		OccurredAt: now,
		// Plan: "" intencional — gym sigue siendo PlanTrial, no se puede activar.
	})
	if err == nil {
		t.Fatal("expected validation error for activate-with-trial-plan")
	}
}

func TestRecordEvent_ActivateAnnual(t *testing.T) {
	// Standard anual ($7,200 MXN/año): cuando Stripe nos dice que el price
	// recurrente anual se cobró, el gym debe quedar en plan
	// "standard_annual" + status active. Idéntico al mensual salvo por el
	// plan resultante. PeriodEndsAt viene del current_period_end del Stripe
	// subscription y aquí lo verificamos pasthrough sin trampearlo.
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	periodEnd := now.AddDate(1, 0, 0)
	uc := newRecordEvent(t, gyms, events, now)

	out, err := uc.Execute(context.Background(), RecordEventInput{
		GymID:        gymID,
		Provider:     subDomain.ProviderStripe,
		Type:         subDomain.EventActivated,
		ExternalID:   "evt_annual_001",
		Plan:         gymDomain.PlanStandardAnnual,
		PeriodEndsAt: &periodEnd,
		OccurredAt:   now,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Applied {
		t.Fatalf("expected Applied=true")
	}
	g := gyms.byID[gymID]
	if g.SubscriptionPlan != gymDomain.PlanStandardAnnual {
		t.Fatalf("plan = %q, want %q", g.SubscriptionPlan, gymDomain.PlanStandardAnnual)
	}
	if g.SubscriptionStatus != gymDomain.StatusActive {
		t.Fatalf("status = %q, want active", g.SubscriptionStatus)
	}
	if g.SubscriptionEndsAt == nil || !g.SubscriptionEndsAt.Equal(periodEnd) {
		t.Fatalf("subscription_ends_at = %v, want %v", g.SubscriptionEndsAt, periodEnd)
	}
}

func TestRecordEvent_VoucherEmittedDoesNotMutateGym(t *testing.T) {
	// Cuando Stripe emite la ficha OXXO, el dueño todavía no pagó. El
	// gym debe quedar EXACTAMENTE igual (sigue trial). El evento se
	// persiste para que el dashboard infiera "ficha pendiente".
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	uc := newRecordEvent(t, gyms, events, now)

	before := *gyms.byID[gymID]
	out, err := uc.Execute(context.Background(), RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderStripe,
		Type:       subDomain.EventVoucherEmitted,
		ExternalID: "evt_voucher_emit",
		Plan:       gymDomain.PlanStandardAnnual,
		OccurredAt: now,
		RawPayload: map[string]any{"voucher_url": "https://stripe.com/oxxo/abc"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Applied {
		t.Fatalf("expected Applied=true (event row persisted)")
	}
	after := gyms.byID[gymID]
	if after.SubscriptionPlan != before.SubscriptionPlan ||
		after.SubscriptionStatus != before.SubscriptionStatus {
		t.Fatalf("voucher_emitted mutated gym state: before=%+v after=%+v", before, *after)
	}
	if len(events.rows) != 1 {
		t.Fatalf("expected 1 event row, got %d", len(events.rows))
	}
}

func TestRecordEvent_VoucherExpiredDoesNotMutateGym(t *testing.T) {
	// El voucher venció sin pagarse. NO regresamos a past_due — ese estado
	// fue diseñado para "tarjeta falló N veces", no para "cliente nunca
	// fue al OXXO". El dashboard infiere "tu ficha venció" del último
	// evento; el cron de billing decide cuándo cancelar.
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	uc := newRecordEvent(t, gyms, events, now)

	before := *gyms.byID[gymID]
	out, err := uc.Execute(context.Background(), RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderStripe,
		Type:       subDomain.EventVoucherExpired,
		ExternalID: "evt_voucher_exp",
		Plan:       gymDomain.PlanStandardAnnual,
		OccurredAt: now,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Applied {
		t.Fatalf("expected Applied=true")
	}
	after := gyms.byID[gymID]
	if after.SubscriptionStatus != before.SubscriptionStatus {
		t.Fatalf("voucher_expired changed status from %q to %q",
			before.SubscriptionStatus, after.SubscriptionStatus)
	}
}

func TestRecordEvent_VoucherEmittedThenActivatedFlipsToAnnual(t *testing.T) {
	// El happy path completo: ficha emitida (no-op) → cliente paga en OXXO
	// → payment_intent.succeeded → activated con plan anual + period
	// extendido un año desde el pago.
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	emitAt := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	paidAt := time.Date(2026, 5, 22, 11, 0, 0, 0, time.UTC)
	uc := newRecordEvent(t, gyms, events, paidAt)

	if _, err := uc.Execute(context.Background(), RecordEventInput{
		GymID: gymID, Provider: subDomain.ProviderStripe,
		Type: subDomain.EventVoucherEmitted, ExternalID: "cs_001",
		Plan: gymDomain.PlanStandardAnnual, OccurredAt: emitAt,
	}); err != nil {
		t.Fatalf("emit err: %v", err)
	}
	if gyms.byID[gymID].SubscriptionPlan != gymDomain.PlanTrial {
		t.Fatalf("after emit: expected plan=trial, got %q", gyms.byID[gymID].SubscriptionPlan)
	}

	periodEnd := paidAt.AddDate(1, 0, 0)
	if _, err := uc.Execute(context.Background(), RecordEventInput{
		GymID: gymID, Provider: subDomain.ProviderStripe,
		Type: subDomain.EventActivated, ExternalID: "pi_001",
		Plan: gymDomain.PlanStandardAnnual, OccurredAt: paidAt,
		PeriodEndsAt: &periodEnd,
	}); err != nil {
		t.Fatalf("activate err: %v", err)
	}
	g := gyms.byID[gymID]
	if g.SubscriptionPlan != gymDomain.PlanStandardAnnual {
		t.Fatalf("after pay: expected plan=standard_annual, got %q", g.SubscriptionPlan)
	}
	if g.SubscriptionStatus != gymDomain.StatusActive {
		t.Fatalf("after pay: expected active, got %q", g.SubscriptionStatus)
	}
	if g.SubscriptionEndsAt == nil || !g.SubscriptionEndsAt.Equal(periodEnd) {
		t.Fatalf("after pay: ends_at = %v, want %v", g.SubscriptionEndsAt, periodEnd)
	}
}

func TestRecordEvent_MissingExternalID(t *testing.T) {
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	uc := newRecordEvent(t, gyms, events, time.Now())

	_, err := uc.Execute(context.Background(), RecordEventInput{
		GymID:    gymID,
		Provider: subDomain.ProviderStripe,
		Type:     subDomain.EventActivated,
		// ExternalID intentionally empty.
		Plan: gymDomain.PlanStandardMonthly,
	})
	if err == nil {
		t.Fatal("expected validation error for empty external_id")
	}
}

// ── Sync touch (bug jul-2026: el status pagado no llegaba al sidecar) ────
//
// Los webhooks escriben la tabla gyms cloud-side sin pasar por el push
// pipeline; sin el TouchGym la fila nunca entra al pull incremental y el
// desktop se queda mostrando el trial. Estos tests fijan CUÁNDO se bumpea:
// todo evento que cambia la suscripción sí, voucher_* y replays no.

type fakeSyncTouch struct{ touched []uuid.UUID }

func (f *fakeSyncTouch) TouchGym(_ context.Context, _ sharedDomain.Transaction, gymID uuid.UUID) error {
	f.touched = append(f.touched, gymID)
	return nil
}

func TestRecordEvent_Activate_TouchesGymSync(t *testing.T) {
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	uc := newRecordEvent(t, gyms, events, now)
	touch := &fakeSyncTouch{}
	uc.SyncTouch = touch

	_, err := uc.Execute(context.Background(), RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderStripe,
		Type:       subDomain.EventActivated,
		ExternalID: "evt_touch_1",
		Plan:       gymDomain.PlanStandardMonthly,
		OccurredAt: now,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(touch.touched) != 1 || touch.touched[0] != gymID {
		t.Fatalf("TouchGym llamadas = %v, want exactamente [%s]", touch.touched, gymID)
	}
}

func TestRecordEvent_CancelledYTrialExtended_TouchesGymSync(t *testing.T) {
	cases := []struct {
		name string
		in   RecordEventInput
	}{
		{"cancelled", RecordEventInput{
			Provider: subDomain.ProviderStripe, Type: subDomain.EventCancelled,
			ExternalID: "evt_cancel",
		}},
		{"trial_extended", RecordEventInput{
			Provider: subDomain.ProviderManual, Type: subDomain.EventTrialExtended,
			ExternalID: "evt_extend", RawPayload: map[string]any{"days": float64(7)},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gyms, gymID := newTrialGym(t)
			events := &fakeEvents{}
			now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
			uc := newRecordEvent(t, gyms, events, now)
			touch := &fakeSyncTouch{}
			uc.SyncTouch = touch

			tc.in.GymID = gymID
			tc.in.OccurredAt = now
			if _, err := uc.Execute(context.Background(), tc.in); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(touch.touched) != 1 {
				t.Fatalf("TouchGym llamadas = %d, want 1", len(touch.touched))
			}
		})
	}
}

func TestRecordEvent_VoucherEmitted_NoTouch(t *testing.T) {
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	uc := newRecordEvent(t, gyms, events, now)
	touch := &fakeSyncTouch{}
	uc.SyncTouch = touch

	_, err := uc.Execute(context.Background(), RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderMercadoPago,
		Type:       subDomain.EventVoucherEmitted,
		ExternalID: "evt_voucher",
		OccurredAt: now,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(touch.touched) != 0 {
		t.Fatalf("voucher_emitted no cambia status — TouchGym llamadas = %d, want 0", len(touch.touched))
	}
}

func TestRecordEvent_IdempotentReplay_NoTouch(t *testing.T) {
	gyms, gymID := newTrialGym(t)
	events := &fakeEvents{}
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	uc := newRecordEvent(t, gyms, events, now)
	touch := &fakeSyncTouch{}
	uc.SyncTouch = touch

	in := RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderStripe,
		Type:       subDomain.EventActivated,
		ExternalID: "evt_replay",
		Plan:       gymDomain.PlanStandardMonthly,
		OccurredAt: now,
	}
	if _, err := uc.Execute(context.Background(), in); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := uc.Execute(context.Background(), in); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(touch.touched) != 1 {
		t.Fatalf("el replay idempotente no debe re-bumpear — TouchGym llamadas = %d, want 1", len(touch.touched))
	}
}
