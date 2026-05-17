package challenge_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
)

func validDates() (time.Time, time.Time, time.Time, time.Time) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	return now.Add(7 * 24 * time.Hour), // starts in 1 week
		now.Add(21 * 24 * time.Hour), // T0 deadline in 3 weeks
		now.Add(91 * 24 * time.Hour), // T1 starts in 13 weeks
		now.Add(105 * 24 * time.Hour) // ends in 15 weeks
}

func TestNewChallenge_HappyPath(t *testing.T) {
	starts, t0, t1, ends := validDates()
	c, err := challenge.NewChallenge(uuid.New(), uuid.New(), "Reto 12", "Primera edición", starts, t0, t1, ends, time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Status != challenge.StatusDraft {
		t.Errorf("Status = %q, want draft", c.Status)
	}
	if c.StrengthCapPct != challenge.DefaultStrengthCapPct {
		t.Errorf("StrengthCapPct = %v, want default %v", c.StrengthCapPct, challenge.DefaultStrengthCapPct)
	}
	if c.TieMarginIR != challenge.DefaultTieMarginIR {
		t.Errorf("TieMarginIR = %v, want default %v", c.TieMarginIR, challenge.DefaultTieMarginIR)
	}
	if c.Version != 1 {
		t.Errorf("Version = %d, want 1", c.Version)
	}
}

func TestNewChallenge_RejectsEmptyName(t *testing.T) {
	starts, t0, t1, ends := validDates()
	_, err := challenge.NewChallenge(uuid.New(), uuid.New(), "   ", "", starts, t0, t1, ends, time.Now().UTC())
	if err != challengeErrors.ErrNameRequired {
		t.Errorf("error = %v, want ErrNameRequired", err)
	}
}

func TestNewChallenge_RejectsBadDates(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name                 string
		starts, t0, t1, ends time.Time
	}{
		{
			name:   "T0 before start",
			starts: now.Add(30 * time.Hour), t0: now, t1: now.Add(60 * time.Hour), ends: now.Add(90 * time.Hour),
		},
		{
			name:   "T1 before T0",
			starts: now, t0: now.Add(60 * time.Hour), t1: now.Add(30 * time.Hour), ends: now.Add(90 * time.Hour),
		},
		{
			name:   "ends before T1",
			starts: now, t0: now.Add(30 * time.Hour), t1: now.Add(90 * time.Hour), ends: now.Add(60 * time.Hour),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := challenge.NewChallenge(uuid.New(), uuid.New(), "Reto", "", tc.starts, tc.t0, tc.t1, tc.ends, now)
			if err != challengeErrors.ErrInvalidDates {
				t.Errorf("error = %v, want ErrInvalidDates", err)
			}
		})
	}
}

func TestStateMachine_HappyPath(t *testing.T) {
	starts, t0, t1, ends := validDates()
	now := time.Now().UTC()
	c, _ := challenge.NewChallenge(uuid.New(), uuid.New(), "Reto", "", starts, t0, t1, ends, now)
	if err := c.OpenRegistration(now); err != nil {
		t.Fatalf("OpenRegistration: %v", err)
	}
	if c.Status != challenge.StatusOpenRegistration {
		t.Errorf("after OpenRegistration Status = %q", c.Status)
	}
	if c.Version != 2 {
		t.Errorf("Version = %d, want 2", c.Version)
	}
	if err := c.StartRunning(now); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	if err := c.StartMeasuringT1(now); err != nil {
		t.Fatalf("StartMeasuringT1: %v", err)
	}
	if err := c.Close(now); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c.Status != challenge.StatusClosed {
		t.Errorf("after Close Status = %q", c.Status)
	}
}

func TestStateMachine_RejectsInvalidTransitions(t *testing.T) {
	starts, t0, t1, ends := validDates()
	now := time.Now().UTC()
	c, _ := challenge.NewChallenge(uuid.New(), uuid.New(), "Reto", "", starts, t0, t1, ends, now)
	// draft → running (skip open_registration) should fail
	if err := c.StartRunning(now); err == nil {
		t.Errorf("StartRunning from draft should have failed")
	}
	// Now cancel and try anything else
	if err := c.Cancel(now); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := c.OpenRegistration(now); err == nil {
		t.Errorf("OpenRegistration from cancelled should have failed")
	}
	if err := c.Cancel(now); err == nil {
		t.Errorf("Cancel from cancelled should have failed (idempotent rejection)")
	}
}

func TestApplyConfig_RejectsOnceRunning(t *testing.T) {
	starts, t0, t1, ends := validDates()
	now := time.Now().UTC()
	c, _ := challenge.NewChallenge(uuid.New(), uuid.New(), "Reto", "", starts, t0, t1, ends, now)
	_ = c.OpenRegistration(now)
	_ = c.StartRunning(now)
	newCap := 30.0
	err := c.ApplyConfig(challenge.ConfigUpdate{StrengthCapPct: &newCap}, now)
	if err != challengeErrors.ErrConfigLocked {
		t.Errorf("error = %v, want ErrConfigLocked", err)
	}
}

func TestApplyConfig_HappyPath(t *testing.T) {
	starts, t0, t1, ends := validDates()
	now := time.Now().UTC()
	c, _ := challenge.NewChallenge(uuid.New(), uuid.New(), "Reto", "", starts, t0, t1, ends, now)
	newName := "Reto 12 — Edición 1"
	newCap := 30.0
	newMargin := 6.5
	err := c.ApplyConfig(challenge.ConfigUpdate{
		Name:           &newName,
		StrengthCapPct: &newCap,
		TieMarginIR:    &newMargin,
	}, now)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if c.Name != newName {
		t.Errorf("Name = %q, want %q", c.Name, newName)
	}
	if c.StrengthCapPct != newCap {
		t.Errorf("StrengthCapPct = %v, want %v", c.StrengthCapPct, newCap)
	}
	if c.TieMarginIR != newMargin {
		t.Errorf("TieMarginIR = %v, want %v", c.TieMarginIR, newMargin)
	}
	if c.Version != 2 {
		t.Errorf("Version = %d, want 2 (single touch from ApplyConfig)", c.Version)
	}
}

func TestApplyConfig_RejectsBadValues(t *testing.T) {
	starts, t0, t1, ends := validDates()
	now := time.Now().UTC()
	c, _ := challenge.NewChallenge(uuid.New(), uuid.New(), "Reto", "", starts, t0, t1, ends, now)
	cases := []struct {
		name string
		u    challenge.ConfigUpdate
	}{
		{"negative fee", challenge.ConfigUpdate{InscriptionFeeCents: ptr(-100)}},
		{"weekly attendance over 7", challenge.ConfigUpdate{MinWeeklyAttendance: ptr(10)}},
		{"negative cap", challenge.ConfigUpdate{StrengthCapPct: fptr(-1)}},
		{"empty name", challenge.ConfigUpdate{Name: sptr("   ")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.ApplyConfig(tc.u, now); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestWindowPredicates(t *testing.T) {
	starts, t0, t1, ends := validDates()
	now := starts.Add(-1 * time.Hour)
	c, _ := challenge.NewChallenge(uuid.New(), uuid.New(), "Reto", "", starts, t0, t1, ends, now)

	// Draft: no captures allowed
	if c.AllowsT0Capture(starts.Add(time.Hour)) {
		t.Errorf("draft must not allow T0 capture")
	}

	_ = c.OpenRegistration(now)
	// During registration + before deadline: T0 yes, T1 no
	if !c.AllowsT0Capture(starts.Add(time.Hour)) {
		t.Errorf("open_registration before T0 deadline should allow T0")
	}
	if c.AllowsT0Capture(t0.Add(time.Hour)) {
		t.Errorf("after T0 deadline should not allow T0")
	}
	if c.AllowsT1Capture(t1.Add(time.Hour)) {
		t.Errorf("open_registration should not allow T1")
	}

	_ = c.StartRunning(now)
	_ = c.StartMeasuringT1(now)
	// measuring_t1 + inside window: T1 yes
	if !c.AllowsT1Capture(t1.Add(time.Hour)) {
		t.Errorf("measuring_t1 inside window should allow T1")
	}
	if c.AllowsT1Capture(ends.Add(time.Hour)) {
		t.Errorf("after ends should not allow T1")
	}
}

func ptr(i int) *int          { return &i }
func fptr(f float64) *float64 { return &f }
func sptr(s string) *string   { return &s }
