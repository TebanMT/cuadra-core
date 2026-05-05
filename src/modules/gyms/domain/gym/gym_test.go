package gym_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
)

func TestNewTrialGym(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	g := gymDomain.NewTrialGym(uuid.New(), 30, now)
	if g.SubscriptionPlan != gymDomain.PlanTrial {
		t.Errorf("plan = %s, want trial", g.SubscriptionPlan)
	}
	if g.TrialEndsAt == nil || !g.TrialEndsAt.Equal(now.Add(30*24*time.Hour)) {
		t.Errorf("trial_ends_at = %v, want +30d", g.TrialEndsAt)
	}
	if g.IsSetupComplete() {
		t.Errorf("expected setup incomplete")
	}
}

func TestUpdateBasicInfoValidation(t *testing.T) {
	now := time.Now().UTC()
	g := gymDomain.NewTrialGym(uuid.New(), 30, now)

	if err := g.UpdateBasicInfo("Gym Bros", "Querétaro, Qro.", "+524421234567"); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if err := g.UpdateBasicInfo("", "", ""); err == nil {
		t.Errorf("empty name should fail")
	}
	if err := g.UpdateBasicInfo("Gym", "QRO", "abc"); err == nil {
		t.Errorf("bad whatsapp should fail")
	}
}

func TestUpdatePaymentMethods(t *testing.T) {
	now := time.Now().UTC()
	g := gymDomain.NewTrialGym(uuid.New(), 30, now)
	if err := g.UpdatePaymentMethods([]string{"cash", "transfer"}); err != nil {
		t.Fatalf("got %v", err)
	}
	if len(g.PaymentMethods) != 2 {
		t.Errorf("len = %d", len(g.PaymentMethods))
	}
	if err := g.UpdatePaymentMethods([]string{"crypto"}); err == nil {
		t.Errorf("expected unsupported method error")
	}
	if err := g.UpdatePaymentMethods([]string{}); err == nil {
		t.Errorf("expected empty methods error")
	}
}

func TestCompleteSetup(t *testing.T) {
	now := time.Now().UTC()
	g := gymDomain.NewTrialGym(uuid.New(), 30, now)
	if err := g.CompleteSetup(true, now); err == nil {
		t.Errorf("should fail without name")
	}
	_ = g.UpdateBasicInfo("Gym Bros", "QRO", "")
	if err := g.CompleteSetup(false, now); err == nil {
		t.Errorf("should fail without membership type")
	}
	_ = g.UpdatePaymentMethods([]string{"cash"})
	if err := g.CompleteSetup(true, now); err != nil {
		t.Fatalf("got %v", err)
	}
	if !g.IsSetupComplete() {
		t.Errorf("expected complete")
	}
}

func TestProfileUpdateRFC(t *testing.T) {
	now := time.Now().UTC()
	g := gymDomain.NewTrialGym(uuid.New(), 30, now)
	good := "MARE850101AAA"
	if err := g.ApplyProfileUpdate(gymDomain.ProfileUpdate{RFC: &good}); err != nil {
		t.Fatalf("good RFC: %v", err)
	}
	bad := "not-an-rfc"
	if err := g.ApplyProfileUpdate(gymDomain.ProfileUpdate{RFC: &bad}); err == nil {
		t.Errorf("bad RFC should fail")
	}
}

func TestProfileUpdateKioskSettings(t *testing.T) {
	now := time.Now().UTC()
	g := gymDomain.NewTrialGym(uuid.New(), 30, now)
	vol := 55
	ttl := 2500
	if err := g.ApplyProfileUpdate(gymDomain.ProfileUpdate{KioskVolume: &vol, KioskFeedbackTTLMs: &ttl}); err != nil {
		t.Fatalf("kiosk update: %v", err)
	}
	if got, _ := g.KioskSettings["audio_volume"].(int); got != 55 {
		t.Errorf("audio_volume = %v, want 55", g.KioskSettings["audio_volume"])
	}
	if got, _ := g.KioskSettings["feedback_ttl_ms"].(int); got != 2500 {
		t.Errorf("feedback_ttl_ms = %v, want 2500", g.KioskSettings["feedback_ttl_ms"])
	}

	bad := 200
	if err := g.ApplyProfileUpdate(gymDomain.ProfileUpdate{KioskVolume: &bad}); err == nil {
		t.Errorf("vol > 100 should fail")
	}
	tooSmall := 100
	if err := g.ApplyProfileUpdate(gymDomain.ProfileUpdate{KioskFeedbackTTLMs: &tooSmall}); err == nil {
		t.Errorf("ttl < 500 should fail")
	}

	// Nil KioskSettings (loaded from a row that never had one) must still
	// accept the update — the mutator allocates the map on demand.
	g2 := gymDomain.NewTrialGym(uuid.New(), 30, now)
	g2.KioskSettings = nil
	if err := g2.ApplyProfileUpdate(gymDomain.ProfileUpdate{KioskVolume: &vol}); err != nil {
		t.Fatalf("nil map: %v", err)
	}
	if g2.KioskSettings["audio_volume"] != 55 {
		t.Errorf("expected audio_volume seeded, got %v", g2.KioskSettings)
	}
}

func TestNextSetupStep(t *testing.T) {
	now := time.Now().UTC()
	g := gymDomain.NewTrialGym(uuid.New(), 30, now)
	if got := g.NextSetupStep(false); got != 2 {
		t.Errorf("step = %d, want 2", got)
	}
	_ = g.UpdateBasicInfo("Gym", "", "")
	if got := g.NextSetupStep(false); got != 3 {
		t.Errorf("step = %d, want 3", got)
	}
	if got := g.NextSetupStep(true); got != 4 {
		t.Errorf("step = %d, want 4", got)
	}
	_ = g.UpdatePaymentMethods([]string{"cash"})
	if got := g.NextSetupStep(true); got != 5 {
		t.Errorf("step = %d, want 5", got)
	}
}
