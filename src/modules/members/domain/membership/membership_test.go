package membership_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
)

func mustType(t *testing.T, durationDays int) *mtDomain.MembershipType {
	t.Helper()
	now := time.Now().UTC()
	mt, err := mtDomain.New(uuid.New(), uuid.New(), "Mensual", 500, durationDays, nil, 0, 0, "", now)
	if err != nil {
		t.Fatalf("type: %v", err)
	}
	return mt
}

// mustTypeMonths construye un plan "mensual natural" (durationMonths>0).
// Útil para los tests que verifican la nueva semántica de expiry por
// meses naturales en lugar de días corridos.
func mustTypeMonths(t *testing.T, months int) *mtDomain.MembershipType {
	t.Helper()
	now := time.Now().UTC()
	mt, err := mtDomain.New(uuid.New(), uuid.New(), "Mensual", 500, months*30, &months, 0, 0, "", now)
	if err != nil {
		t.Fatalf("type meses: %v", err)
	}
	return mt
}

func TestNewMembership(t *testing.T) {
	mt := mustType(t, 30)
	now := time.Now().UTC()
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	ms := membership.New(uuid.New(), uuid.New(), uuid.New(), mt, start, now)
	if !ms.StartDate.Equal(start) {
		t.Errorf("start = %v", ms.StartDate)
	}
	wantExpiry := start.AddDate(0, 0, 30)
	if !ms.ExpiryDate.Equal(wantExpiry) {
		t.Errorf("expiry = %v, want %v", ms.ExpiryDate, wantExpiry)
	}
	if ms.PriceSnapshot != 500 || ms.DurationDaysSnapshot != 30 || ms.TypeNameSnapshot != "Mensual" {
		t.Errorf("snapshot mismatch: %+v", ms)
	}
}

// UC-018: la regla de Renew. Si vigencia >= payment_date, acumula. Si vencida,
// reinicia desde payment_date.
func TestRenewBeforeExpiry_Accumulates(t *testing.T) {
	mt := mustType(t, 30)
	now := time.Now().UTC()
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	current := membership.New(uuid.New(), mt.GymID, uuid.New(), mt, start, now)
	// Pagamos el 20 abr — current expira 1 may.
	paymentDate := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	next := current.Renew(uuid.New(), mt, paymentDate, now)
	wantStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	wantExpiry := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	if !next.StartDate.Equal(wantStart) {
		t.Errorf("start = %v, want %v", next.StartDate, wantStart)
	}
	if !next.ExpiryDate.Equal(wantExpiry) {
		t.Errorf("expiry = %v, want %v", next.ExpiryDate, wantExpiry)
	}
}

func TestRenewAfterExpiry_RestartsFromPaymentDate(t *testing.T) {
	mt := mustType(t, 30)
	now := time.Now().UTC()
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	current := membership.New(uuid.New(), mt.GymID, uuid.New(), mt, start, now)
	// Pagamos el 15 may — current expira 1 may, ya vencido.
	paymentDate := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	next := current.Renew(uuid.New(), mt, paymentDate, now)
	if !next.StartDate.Equal(paymentDate) {
		t.Errorf("start = %v, want %v", next.StartDate, paymentDate)
	}
	wantExpiry := paymentDate.AddDate(0, 0, 30)
	if !next.ExpiryDate.Equal(wantExpiry) {
		t.Errorf("expiry = %v, want %v", next.ExpiryDate, wantExpiry)
	}
}

func TestMarkReplaced(t *testing.T) {
	mt := mustType(t, 30)
	now := time.Now().UTC()
	current := membership.New(uuid.New(), uuid.New(), uuid.New(), mt, now, now)
	replacement := uuid.New()
	current.MarkReplaced(replacement, now)
	if current.Status != membership.StatusReplaced {
		t.Errorf("status = %q", current.Status)
	}
	if current.ReplacedBy == nil || *current.ReplacedBy != replacement {
		t.Errorf("replaced_by = %v", current.ReplacedBy)
	}
}

func TestAdjustExpiry(t *testing.T) {
	mt := mustType(t, 30)
	now := time.Now().UTC()
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	ms := membership.New(uuid.New(), uuid.New(), uuid.New(), mt, start, now)
	prev, next, err := ms.AdjustExpiry(14, now)
	if err != nil {
		t.Fatalf("adjust +14: %v", err)
	}
	if !prev.Equal(start.AddDate(0, 0, 30)) {
		t.Errorf("prev = %v", prev)
	}
	if !next.Equal(start.AddDate(0, 0, 44)) {
		t.Errorf("next = %v", next)
	}
	// Cannot push expiry behind start_date.
	if _, _, err := ms.AdjustExpiry(-100, now); err == nil {
		t.Errorf("adjust before start should fail")
	}
}

func TestSetExpiry(t *testing.T) {
	mt := mustType(t, 30)
	now := time.Now().UTC()
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	ms := membership.New(uuid.New(), uuid.New(), uuid.New(), mt, start, now)
	target := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	prev, days, err := ms.SetExpiry(target, now)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !prev.Equal(start.AddDate(0, 0, 30)) {
		t.Errorf("prev = %v", prev)
	}
	if days != 45 {
		t.Errorf("days = %d, want 45", days)
	}
	if !ms.ExpiryDate.Equal(target) {
		t.Errorf("expiry = %v", ms.ExpiryDate)
	}
}

func TestNewAdjustment_ReasonValidation(t *testing.T) {
	now := time.Now().UTC()
	prev := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	next := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	if _, err := membership.NewAdjustment(uuid.New(), uuid.New(), uuid.New(), uuid.New(),
		"abc", 14, prev, next, now); err == nil {
		t.Errorf("3-char reason should fail (need ≥5)")
	}
	if _, err := membership.NewAdjustment(uuid.New(), uuid.New(), uuid.New(), uuid.New(),
		"Cortesía COVID", 14, prev, next, now); err != nil {
		t.Errorf("valid reason failed: %v", err)
	}
}

// TestNewMembership_Mensual_UsaMesNatural reproduce el bug que reportó
// el dueño: una mensual vendida el 25-may debe vencer el 25-jun (1 mes
// natural), NO el 24-jun (30 días corridos).
func TestNewMembership_Mensual_UsaMesNatural(t *testing.T) {
	mt := mustTypeMonths(t, 1)
	now := time.Now().UTC()
	start := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	ms := membership.New(uuid.New(), uuid.New(), uuid.New(), mt, start, now)
	want := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	if !ms.ExpiryDate.Equal(want) {
		t.Errorf("expiry = %v, want %v (1 mes natural)", ms.ExpiryDate, want)
	}
	if ms.DurationMonthsSnapshot == nil || *ms.DurationMonthsSnapshot != 1 {
		t.Errorf("snapshot meses = %v, want 1", ms.DurationMonthsSnapshot)
	}
}

// TestNewMembership_Anual_UsaAnoNatural: socio paga 14-feb anual,
// debe vencer 14-feb del año siguiente, no 14-feb +365d.
func TestNewMembership_Anual_UsaAnoNatural(t *testing.T) {
	mt := mustTypeMonths(t, 12)
	now := time.Now().UTC()
	start := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC)
	ms := membership.New(uuid.New(), uuid.New(), uuid.New(), mt, start, now)
	want := time.Date(2027, 2, 14, 0, 0, 0, 0, time.UTC)
	if !ms.ExpiryDate.Equal(want) {
		t.Errorf("expiry = %v, want %v (1 año natural)", ms.ExpiryDate, want)
	}
}

// TestNewMembership_Mensual_FinDeMes: socio paga el 31-ene mensual,
// debe vencer el 28-feb (o 29 en bisiesto), NO 3-mar (overflow nativo
// de Go AddDate).
func TestNewMembership_Mensual_FinDeMes(t *testing.T) {
	mt := mustTypeMonths(t, 1)
	now := time.Now().UTC()
	// 2026 NO es bisiesto → febrero tiene 28 días.
	start := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	ms := membership.New(uuid.New(), uuid.New(), uuid.New(), mt, start, now)
	want := time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)
	if !ms.ExpiryDate.Equal(want) {
		t.Errorf("expiry = %v, want %v (clamp al último día de feb)", ms.ExpiryDate, want)
	}
}

// TestNewMembership_Personalizada_SigueUsandoDias: un plan custom de 45
// días debe seguir calculando expiry como start + 45 días.
func TestNewMembership_Personalizada_SigueUsandoDias(t *testing.T) {
	mt := mustType(t, 45)
	now := time.Now().UTC()
	start := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	ms := membership.New(uuid.New(), uuid.New(), uuid.New(), mt, start, now)
	want := start.AddDate(0, 0, 45)
	if !ms.ExpiryDate.Equal(want) {
		t.Errorf("expiry = %v, want %v (45 días corridos)", ms.ExpiryDate, want)
	}
	if ms.DurationMonthsSnapshot != nil {
		t.Errorf("snapshot meses debería ser nil para personalizada, got %v", *ms.DurationMonthsSnapshot)
	}
}

// TestRenew_Mensual_AcumulaPorMeses: renovación temprana de mensual,
// el nuevo expiry debe ser ExpiryDate previo + 1 mes natural (no +30d).
func TestRenew_Mensual_AcumulaPorMeses(t *testing.T) {
	mt := mustTypeMonths(t, 1)
	now := time.Now().UTC()
	start := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	current := membership.New(uuid.New(), mt.GymID, uuid.New(), mt, start, now)
	// Pagamos antes del vencimiento (current expira 25-jun).
	paymentDate := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	next := current.Renew(uuid.New(), mt, paymentDate, now)
	// newStart = expiry previo = 25-jun. newExpiry = 25-jul.
	wantStart := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	wantExpiry := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	if !next.StartDate.Equal(wantStart) {
		t.Errorf("renew start = %v, want %v", next.StartDate, wantStart)
	}
	if !next.ExpiryDate.Equal(wantExpiry) {
		t.Errorf("renew expiry = %v, want %v", next.ExpiryDate, wantExpiry)
	}
}

func TestIsActiveAndDays(t *testing.T) {
	mt := mustType(t, 30)
	now := time.Now().UTC()
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	ms := membership.New(uuid.New(), uuid.New(), uuid.New(), mt, start, now)
	today := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	if !ms.IsActive(today) {
		t.Errorf("should be active on 2026-04-25 (expiry 2026-05-01)")
	}
	if days := ms.DaysUntilExpiry(today); days != 6 {
		t.Errorf("days = %d, want 6", days)
	}
	expired := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	if ms.IsActive(expired) {
		t.Errorf("should NOT be active after expiry")
	}
}
