package scoring_test

import (
	"math"
	"testing"

	"github.com/cuadra/cuadra-core/src/modules/challenges/domain/scoring"
)

// approxEqual compares floats with a tolerance that accommodates the
// rounding of intermediate steps in the formula. 1e-6 keeps us strict
// enough to catch real bugs but tolerant of fp noise on intermediate
// divisions.
func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// TestCalculateIR_MariaCase mirrors the canonical example from the public
// landing page so any visible regression on the FE matches the BE math.
//
//	T₀: 75 kg @ 30% BF → fatMass=22.5, leanMass=52.5
//	T₁: 72 kg @ 25% BF → fatMass=18.0, leanMass=54.0
//	ΔG% = (22.5-18)/22.5 = 20%
//	ΔM% = (54-52.5)/52.5 ≈ 2.857%, ×2 ≈ 5.714
//
// Lift numbers chosen so that ΔF% lands on a clean 18% with the body-
// weight normalisation factored in (per the landing example).
func TestCalculateIR_MariaCase(t *testing.T) {
	t0 := scoring.Measurement{
		BodyWeight: 75, BodyFatPct: 30,
		Legs1RM: 120, Push1RM: 60, Pull1RM: 90, // F₀ = 270/75 = 3.6
	}
	t1 := scoring.Measurement{
		BodyWeight: 72, BodyFatPct: 25,
		// F₁ = 4.248 = 3.6 × 1.18 → total lifts = 305.856 at 72 kg
		Legs1RM: 135.856, Push1RM: 70, Pull1RM: 100,
	}
	got := scoring.CalculateIR(t0, t1, 25)

	if !approxEqual(got.DeltaFatPct, 20, 1e-6) {
		t.Errorf("DeltaFatPct = %v, want ≈ 20", got.DeltaFatPct)
	}
	if !approxEqual(got.DeltaMusclePct, 2.857142857, 1e-6) {
		t.Errorf("DeltaMusclePct = %v, want ≈ 2.857", got.DeltaMusclePct)
	}
	if !approxEqual(got.DeltaStrengthPct, 18, 0.01) {
		t.Errorf("DeltaStrengthPct = %v, want ≈ 18", got.DeltaStrengthPct)
	}
	wantIR := 20 + 2*2.857142857 + 18 // ≈ 43.714
	if !approxEqual(got.IR, wantIR, 0.05) {
		t.Errorf("IR = %v, want ≈ %v", got.IR, wantIR)
	}
}

// TestCalculateIR_StrengthCapped — a beginner-style gain (50%+ ΔF%) must
// be clamped at the cap. Without this, novatadas dominate; the cap is the
// equaliser between newbies and intermediates.
func TestCalculateIR_StrengthCapped(t *testing.T) {
	t0 := scoring.Measurement{
		BodyWeight: 80, BodyFatPct: 25,
		Legs1RM: 60, Push1RM: 40, Pull1RM: 60, // F₀ = 160/80 = 2.0
	}
	t1 := scoring.Measurement{
		BodyWeight: 80, BodyFatPct: 25,
		Legs1RM: 100, Push1RM: 60, Pull1RM: 100, // F₁ = 260/80 = 3.25 → ΔF% = 62.5
	}
	got := scoring.CalculateIR(t0, t1, 25)
	if !approxEqual(got.DeltaStrengthPct, 25, 1e-6) {
		t.Errorf("DeltaStrengthPct = %v, want 25 (capped)", got.DeltaStrengthPct)
	}
	// Body comp unchanged → only the (capped) strength term contributes to IR.
	if !approxEqual(got.IR, 25, 1e-6) {
		t.Errorf("IR = %v, want 25", got.IR)
	}
}

// TestCalculateIR_MuscleLoss — a participant who only diets (loses fat
// AND loses muscle) must have ΔM% < 0 pulling their IR down. The 2×
// weight makes the punishment for muscle loss proportionally large,
// which is exactly the incentive the formula encodes.
//
// We hold bodyweight constant (only %BF changes) so the strength term
// stays exactly flat and the muscle term is isolated. With bodyweight
// dropping, the normalised strength would rise and partially mask the
// muscle penalty — interesting case but not what this test is checking.
func TestCalculateIR_MuscleLoss(t *testing.T) {
	t0 := scoring.Measurement{
		BodyWeight: 100, BodyFatPct: 30,
		Legs1RM: 100, Push1RM: 70, Pull1RM: 100,
	}
	t1 := scoring.Measurement{
		BodyWeight: 100, BodyFatPct: 27, // same bodyweight, %BF dropped 3 points
		Legs1RM: 100, Push1RM: 70, Pull1RM: 100, // same absolute lifts → ΔF% = 0
	}
	// fatMass₀ = 30, fatMass₁ = 27 → ΔG% = (30-27)/30 = 10
	// leanMass₀ = 70, leanMass₁ = 73 → ΔM% = (73-70)/70 ≈ 4.286
	// Hmm — same bodyweight + lower %BF actually GAINS muscle. We need a
	// scenario where muscle drops. Flip to: bodyweight drops AND %BF rises.
	t1 = scoring.Measurement{
		BodyWeight: 95, BodyFatPct: 33, // weight down, %BF up
		Legs1RM: 100, Push1RM: 70, Pull1RM: 100,
	}
	// fatMass₀ = 30, fatMass₁ = 31.35 → ΔG% ≈ -4.5 (gained fat in absolute terms)
	// leanMass₀ = 70, leanMass₁ = 63.65 → ΔM% ≈ -9.07 (lost real muscle)
	got := scoring.CalculateIR(t0, t1, 25)
	if !(got.DeltaMusclePct < 0) {
		t.Errorf("DeltaMusclePct = %v, want negative (muscle was lost)", got.DeltaMusclePct)
	}
	if !(got.DeltaFatPct < 0) {
		t.Errorf("DeltaFatPct = %v, want negative (absolute fat went up)", got.DeltaFatPct)
	}
	// The muscle term is doubled, so even though strength rose (lighter
	// bodyweight, same lifts), IR should still be net negative.
	if got.IR >= 0 {
		t.Errorf("IR = %v, want negative — fat gain + muscle loss dominate the modest strength boost", got.IR)
	}
}

// TestCalculateIR_AllZeros — degenerate input must NOT panic or return
// NaN. This guards against bad fixtures, sentinel rows from migrations,
// and any future place that might forget to validate before calling.
func TestCalculateIR_AllZeros(t *testing.T) {
	zero := scoring.Measurement{}
	got := scoring.CalculateIR(zero, zero, 25)
	if !approxEqual(got.DeltaFatPct, 0, 1e-9) ||
		!approxEqual(got.DeltaMusclePct, 0, 1e-9) ||
		!approxEqual(got.DeltaStrengthPct, 0, 1e-9) ||
		!approxEqual(got.IR, 0, 1e-9) {
		t.Errorf("zero input produced non-zero breakdown: %+v", got)
	}
	if math.IsNaN(got.IR) || math.IsInf(got.IR, 0) {
		t.Errorf("IR = %v, want a finite number on zero input", got.IR)
	}
}

// TestCalculateIR_NegativeStrengthNotClamped — losing strength should NOT
// be clamped to 0 by the cap. The cap is upper-only; this guards against
// the very common off-by-one of using math.Min(x, cap) with a negative
// path that accidentally clamps the floor too.
func TestCalculateIR_NegativeStrengthNotClamped(t *testing.T) {
	t0 := scoring.Measurement{BodyWeight: 80, BodyFatPct: 20, Legs1RM: 100, Push1RM: 60, Pull1RM: 90}
	t1 := scoring.Measurement{BodyWeight: 80, BodyFatPct: 20, Legs1RM: 80, Push1RM: 50, Pull1RM: 70}
	got := scoring.CalculateIR(t0, t1, 25)
	if got.DeltaStrengthPct >= 0 {
		t.Errorf("DeltaStrengthPct = %v, want negative (lost strength)", got.DeltaStrengthPct)
	}
	// 250 → 200, ΔF% = -20
	if !approxEqual(got.DeltaStrengthPct, -20, 1e-6) {
		t.Errorf("DeltaStrengthPct = %v, want ≈ -20", got.DeltaStrengthPct)
	}
}

// TestCalculateIR_NoChange — identical T₀ and T₁ must produce IR == 0.
// Sanity test for the "stable participant" case; protects against any
// future refactor that introduces a non-zero baseline.
func TestCalculateIR_NoChange(t *testing.T) {
	m := scoring.Measurement{
		BodyWeight: 80, BodyFatPct: 20,
		Legs1RM: 100, Push1RM: 70, Pull1RM: 100,
	}
	got := scoring.CalculateIR(m, m, 25)
	if !approxEqual(got.IR, 0, 1e-9) {
		t.Errorf("IR = %v on no-change input, want 0", got.IR)
	}
}

// TestCalculateIR_StrengthFromBodyWeightChange — F is normalised per
// bodyweight. A participant who keeps the same absolute lifts but loses
// weight should show a small POSITIVE ΔF% (got "stronger per kg").
func TestCalculateIR_StrengthFromBodyWeightChange(t *testing.T) {
	t0 := scoring.Measurement{BodyWeight: 100, BodyFatPct: 25, Legs1RM: 100, Push1RM: 60, Pull1RM: 90}
	t1 := scoring.Measurement{BodyWeight: 90, BodyFatPct: 25, Legs1RM: 100, Push1RM: 60, Pull1RM: 90}
	got := scoring.CalculateIR(t0, t1, 25)
	// F₀ = 250/100 = 2.5; F₁ = 250/90 ≈ 2.778 → ΔF% ≈ 11.11
	if !approxEqual(got.DeltaStrengthPct, 11.1111111, 1e-4) {
		t.Errorf("DeltaStrengthPct = %v, want ≈ 11.11 (from bodyweight drop alone)", got.DeltaStrengthPct)
	}
}

func TestEstimateOneRepMax_Epley(t *testing.T) {
	cases := []struct {
		name   string
		weight float64
		reps   int
		want   float64
	}{
		{"5×100 → 116.67", 100, 5, 100 * (1 + 5.0/30)},
		{"3×120 → 132", 120, 3, 120 * (1 + 3.0/30)},
		{"1×100 → 103.33", 100, 1, 100 * (1 + 1.0/30)},
		{"zero reps → 0 (defensive)", 100, 0, 0},
		{"zero weight → 0 (defensive)", 0, 5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scoring.EstimateOneRepMax(tc.weight, tc.reps)
			if !approxEqual(got, tc.want, 1e-6) {
				t.Errorf("EstimateOneRepMax(%v, %v) = %v, want %v", tc.weight, tc.reps, got, tc.want)
			}
		})
	}
}
