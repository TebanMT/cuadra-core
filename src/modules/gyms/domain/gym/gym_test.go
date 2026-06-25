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

	if err := g.UpdateBasicInfo("Gym Bros", "Querétaro, Qro."); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if err := g.UpdateBasicInfo("", ""); err == nil {
		t.Errorf("empty name should fail")
	}
	// WhatsApp ya no es input del paso 2 (ADR-009). El campo se conecta
	// post-suscripción a Plus via ApplyProfileUpdate.
	if g.WhatsApp != nil {
		t.Errorf("WhatsApp debe quedar nil tras UpdateBasicInfo, got %v", *g.WhatsApp)
	}
}

// TestUpdateBasicInfo_PreservesExistingWhatsApp — si un gym ya tenía
// WhatsApp conectado (caso de gyms históricos), re-correr UpdateBasicInfo
// NO debe limpiarlo. El paso 2 ahora ignora el campo por completo.
func TestUpdateBasicInfo_PreservesExistingWhatsApp(t *testing.T) {
	now := time.Now().UTC()
	g := gymDomain.NewTrialGym(uuid.New(), 30, now)
	existing := "+524421234567"
	if err := g.ApplyProfileUpdate(gymDomain.ProfileUpdate{WhatsApp: &existing}); err != nil {
		t.Fatalf("seed whatsapp: %v", err)
	}
	if err := g.UpdateBasicInfo("Gym Bros", "QRO"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if g.WhatsApp == nil || *g.WhatsApp != existing {
		t.Errorf("WhatsApp pisado por UpdateBasicInfo: got %v, want %q", g.WhatsApp, existing)
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
	_ = g.UpdateBasicInfo("Gym Bros", "QRO")
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
	_ = g.UpdateBasicInfo("Gym", "")
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

// TestIsAccessHardBlocked cubre la matriz de bloqueo que aplica el sidecar
// para hacer cumplir la decisión "offline tolera, sync exitoso confirma
// cancelación". Importante: el trial vencido localmente NO bloquea (porque
// el cloud aún no ha bajado el flip a cancelled vía sync); sólo el status
// cancelled + grace vencida bloquea.
func TestIsAccessHardBlocked(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	past := now.Add(-72 * time.Hour)
	future := now.Add(72 * time.Hour)

	type tc struct {
		name        string
		plan        string
		status      string
		trialEndsAt *time.Time
		subEndsAt   *time.Time
		want        bool
	}
	cases := []tc{
		{"trial activo, no vencido", gymDomain.PlanTrial, gymDomain.StatusActive, &future, nil, false},
		{"trial activo, ya pasó trial_ends_at (offline indefinido)", gymDomain.PlanTrial, gymDomain.StatusActive, &past, nil, false},
		{"paid activo", gymDomain.PlanStandardMonthly, gymDomain.StatusActive, nil, &future, false},
		{"past_due — sólo warning, no bloqueo", gymDomain.PlanPlusMonthly, gymDomain.StatusPastDue, nil, &past, false},
		{"cancelled con grace vigente", gymDomain.PlanStandardMonthly, gymDomain.StatusCancelled, nil, &future, false},
		{"cancelled con grace vencida → BLOQUEAR", gymDomain.PlanStandardMonthly, gymDomain.StatusCancelled, nil, &past, true},
		{"cancelled sin grace (nil) → BLOQUEAR", gymDomain.PlanPlusMonthly, gymDomain.StatusCancelled, nil, nil, true},
		{"cancelled + grace == now (exactamente venció)", gymDomain.PlanStandardMonthly, gymDomain.StatusCancelled, nil, &now, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := gymDomain.NewTrialGym(uuid.New(), 30, now)
			g.SubscriptionPlan = c.plan
			g.SubscriptionStatus = c.status
			g.TrialEndsAt = c.trialEndsAt
			g.SubscriptionEndsAt = c.subEndsAt
			if got := g.IsAccessHardBlocked(now); got != c.want {
				t.Errorf("IsAccessHardBlocked() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestHasPlusOnlyFields_EmptyWebhookNotPlus — regresión: el desktop manda
// access_webhook_url:"" en cada guardado de perfil; un valor VACÍO no debe
// considerarse acción Plus (antes gateaba a 402 a todo gym Standard).
func TestHasPlusOnlyFields_EmptyWebhookNotPlus(t *testing.T) {
	sp := func(s string) *string { return &s }
	cases := []struct {
		name string
		u    gymDomain.ProfileUpdate
		want bool
	}{
		{"vacío (caso del desktop) → no Plus", gymDomain.ProfileUpdate{AccessWebhookURL: sp("")}, false},
		{"solo espacios → no Plus", gymDomain.ProfileUpdate{AccessWebhookURL: sp("   ")}, false},
		{"nil → no Plus", gymDomain.ProfileUpdate{AccessWebhookURL: nil}, false},
		{"URL real → Plus", gymDomain.ProfileUpdate{AccessWebhookURL: sp("https://x/door")}, true},
		{"secret vacío → no Plus", gymDomain.ProfileUpdate{AccessWebhookSecret: sp("")}, false},
		{"secret real → Plus", gymDomain.ProfileUpdate{AccessWebhookSecret: sp("s3cr3t")}, true},
		{"RFC → Plus", gymDomain.ProfileUpdate{RFC: sp("XAXX010101000")}, true},
		{"solo nombre/horario → no Plus", gymDomain.ProfileUpdate{Name: sp("Gym"), OpenTime: sp("06:00")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.HasPlusOnlyFields(); got != tc.want {
				t.Errorf("HasPlusOnlyFields() = %v, want %v", got, tc.want)
			}
		})
	}
}
