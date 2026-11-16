package app

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type fakeTrialReader struct {
	trials []ExpiredTrial
	asked  time.Time
}

func (r *fakeTrialReader) FindExpiredTrials(_ sharedDomain.Transaction, now time.Time) ([]ExpiredTrial, error) {
	r.asked = now
	return r.trials, nil
}

func TestExpireTrials_RecordsOneCancellationPerTrial(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	a := ExpiredTrial{GymID: uuid.New(), TrialEndsAt: now.Add(-time.Hour)}
	b := ExpiredTrial{GymID: uuid.New(), TrialEndsAt: now.Add(-40 * 24 * time.Hour)}
	reader := &fakeTrialReader{trials: []ExpiredTrial{a, b}}
	recorder := &fakeRecorder{applyAll: true}
	uc := NewExpireTrials(reader, recorder, fakeUoW{})

	closed, err := uc.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if closed != 2 || len(recorder.calls) != 2 {
		t.Fatalf("closed=%d calls=%d, want 2 and 2", closed, len(recorder.calls))
	}
	if !reader.asked.Equal(now) {
		t.Errorf("reader asked with %v, want %v", reader.asked, now)
	}
	for i, trial := range []ExpiredTrial{a, b} {
		call := recorder.calls[i]
		if call.GymID != trial.GymID || call.Type != subDomain.EventCancelled || call.Provider != subDomain.ProviderManual {
			t.Errorf("call %d = %+v", i, call)
		}
		if call.ExternalID != TrialExpiredExternalID(trial) {
			t.Errorf("external id = %q, want %q", call.ExternalID, TrialExpiredExternalID(trial))
		}
	}
}

func TestExpireTrials_ExternalIDChangesWhenTheTrialIsExtended(t *testing.T) {
	gymID := uuid.New()
	first := ExpiredTrial{GymID: gymID, TrialEndsAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	extended := ExpiredTrial{GymID: gymID, TrialEndsAt: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)}
	if TrialExpiredExternalID(first) == TrialExpiredExternalID(extended) {
		t.Fatal("an extended trial needs its own close event")
	}
	if TrialExpiredExternalID(first) != TrialExpiredExternalID(first) {
		t.Fatal("the same trial must replay as a no-op")
	}
}

// RecordEvent: cancelar una prueba le da días de gracia (y después solo
// lectura); cancelar un plan pagado mantiene el bloqueo total de siempre.
func TestRecordEvent_CancelledTrialGetsGraceNotHardBlock(t *testing.T) {
	gyms, gymID := newTrialGym(t)
	trialEnded := time.Date(2026, 11, 13, 11, 0, 0, 0, time.UTC)
	gyms.byID[gymID].TrialEndsAt = &trialEnded
	now := trialEnded.Add(time.Hour)
	uc := newRecordEvent(t, gyms, &fakeEvents{}, now)

	if _, err := uc.Execute(context.Background(), RecordEventInput{
		GymID: gymID, Provider: subDomain.ProviderManual, Type: subDomain.EventCancelled,
		ExternalID: "trial-expired:x", OccurredAt: now,
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	g := gyms.byID[gymID]
	if g.SubscriptionStatus != gymDomain.StatusCancelled || g.SubscriptionPlan != gymDomain.PlanTrial {
		t.Fatalf("gym = %+v", g)
	}
	wantGrace := now.Add(gymDomain.TrialGraceDays * 24 * time.Hour)
	if g.SubscriptionEndsAt == nil || !g.SubscriptionEndsAt.Equal(wantGrace) {
		t.Fatalf("grace ends = %v, want %v", g.SubscriptionEndsAt, wantGrace)
	}
	if g.IsAccessHardBlocked(wantGrace.Add(time.Hour)) || !g.IsTrialReadOnly(wantGrace.Add(time.Hour)) {
		t.Fatal("after the grace an ended trial is read-only, never hard-blocked")
	}
}
