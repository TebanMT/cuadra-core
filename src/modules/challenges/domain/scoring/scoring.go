// Package scoring is the pure mathematical core of the challenges (retos)
// module. CalculateIR computes the Índice de Recomposición from two
// measurements (T₀ and T₁) and a configurable strength cap.
//
// The package is deliberately dependency-free — no DB, no logger, no
// clock — so the formula behaviour is exhaustively testable with table
// tests and the same code paths run cloud-side AND sidecar-side without
// any build-tag gymnastics. A bug here destroys a real $20K event; that
// constraint drove every design call in this file.
//
// Formula (matches Reto 12 documentation and the public landing page):
//
//	IR = ΔG% + 2·ΔM% + min(ΔF%, strengthCap)
//
//	ΔG% = ((G₀ − G₁) / G₀) × 100        — % grasa perdida
//	ΔM% = ((M₁ − M₀) / M₀) × 100        — % músculo ganado
//	ΔF% = ((F₁ − F₀) / F₀) × 100        — % fuerza ganada
//
// where:
//
//	G = body_weight × body_fat_pct / 100     (kg de masa grasa)
//	M = body_weight − G                      (kg de masa magra)
//	F = (Legs1RM + Push1RM + Pull1RM) / body_weight   (fuerza normalizada)
//
// Notes worth keeping in mind for future maintainers:
//
//	1. The strength cap applies only on the upper side. Losing strength
//	   (negative ΔF%) is NOT clamped — it should hurt the participant.
//	2. The 2× weight on ΔM% is intentional. Ganar músculo es ~2x más
//	   difícil que perder grasa en el mismo horizonte; sin el factor, los
//	   participantes que solo bajan grasa dominan. Documentado en el
//	   landing público; cambiarlo rompe el contrato con los inscritos.
//	3. Strength is normalised by body weight at each moment separately.
//	   This way a participant who gained 5 kg of bodyweight while keeping
//	   the same absolute lifts is correctly scored as flat strength, not
//	   stronger.
//	4. Degenerate inputs (any required denominator = 0) return the zero
//	   breakdown instead of NaN / panic / Inf. Ranking treats IR=0 as
//	   valid; participants without a real T₁ never reach scoring in the
//	   first place (the ranking query filters them out upstream).
package scoring

// Measurement is the per-moment snapshot the scoring function consumes.
// Field units are documented inline. The struct is intentionally flat —
// no nested 1RM-per-pattern object — because the formula treats the
// three lifts as a fungible sum and a flat shape is easier to test.
type Measurement struct {
	BodyWeight float64 // kg
	BodyFatPct float64 // 0..100 (NOT 0..1)
	Legs1RM    float64 // kg — already-derived 1RM (Epley applied upstream)
	Push1RM    float64 // kg
	Pull1RM    float64 // kg
}

// ScoreBreakdown is what the function returns. Callers needing the bare
// IR just take .IR; callers rendering a per-component view (Ranking tab
// on the FE, audit log) get the three deltas pre-computed.
//
// DeltaStrengthPct is the POST-cap value — what actually flows into IR.
// The pre-cap value is not returned because keeping both invites bugs
// where the wrong one is read.
type ScoreBreakdown struct {
	DeltaFatPct      float64
	DeltaMusclePct   float64
	DeltaStrengthPct float64
	IR               float64
}

// CalculateIR computes the IR breakdown for a participant whose T₀ and T₁
// measurements are both available. The function is pure: same inputs
// always produce the same outputs, no I/O, no global state.
//
// strengthCap is the maximum ΔF% allowed into IR (per-challenge config,
// default 25.0 for Reto 12 — set on the challenges row). A zero or
// negative cap effectively zeroes out strength gains; negative caps are
// not validated here (the use case rejects them upstream).
//
// Degenerate / zero-baseline inputs are absorbed silently:
//   - fatMass₀ == 0  → ΔG% = 0
//   - leanMass₀ == 0 → ΔM% = 0
//   - bodyWeight₀ ≤ 0 or strength₀ == 0 → ΔF% = 0
//
// In production these never trigger because the use case validates
// body_fat_pct ∈ (3, 60) and body_weight > 0 before persistence. But
// defensive zeroes here mean a bad migration / fixture can never produce
// a NaN in the ranking — at worst it produces a low IR.
func CalculateIR(t0, t1 Measurement, strengthCap float64) ScoreBreakdown {
	fatMass0 := t0.BodyWeight * t0.BodyFatPct / 100
	fatMass1 := t1.BodyWeight * t1.BodyFatPct / 100
	leanMass0 := t0.BodyWeight - fatMass0
	leanMass1 := t1.BodyWeight - fatMass1

	var deltaFat float64
	if fatMass0 > 0 {
		deltaFat = (fatMass0 - fatMass1) / fatMass0 * 100
	}

	var deltaMuscle float64
	if leanMass0 > 0 {
		deltaMuscle = (leanMass1 - leanMass0) / leanMass0 * 100
	}

	var deltaStrength float64
	if t0.BodyWeight > 0 && t1.BodyWeight > 0 {
		strength0 := (t0.Legs1RM + t0.Push1RM + t0.Pull1RM) / t0.BodyWeight
		strength1 := (t1.Legs1RM + t1.Push1RM + t1.Pull1RM) / t1.BodyWeight
		if strength0 > 0 {
			deltaStrength = (strength1 - strength0) / strength0 * 100
			if deltaStrength > strengthCap {
				deltaStrength = strengthCap
			}
		}
	}

	return ScoreBreakdown{
		DeltaFatPct:      deltaFat,
		DeltaMusclePct:   deltaMuscle,
		DeltaStrengthPct: deltaStrength,
		IR:               deltaFat + 2*deltaMuscle + deltaStrength,
	}
}

// EstimateOneRepMax applies the Epley formula to a submaximal set:
//
//	1RM ≈ weight × (1 + reps/30)
//
// Used by the measurement capture path to derive Legs1RM / Push1RM /
// Pull1RM from the raw (weight, reps) pair the nutricionista records.
// Lives here (not on Measurement) so the function stays trivially
// testable and the use case can decide where to materialise the 1RM
// (today: at write time, persisted is the raw weight+reps; the 1RM is
// recomputed at ranking time from the active measurement).
//
// Returns 0 for non-positive weight (defensive — the caller validates
// upstream, but a zero is safer than NaN).
func EstimateOneRepMax(weight float64, reps int) float64 {
	if weight <= 0 || reps <= 0 {
		return 0
	}
	return weight * (1 + float64(reps)/30)
}
