package gym

import (
	"testing"
	"time"
)

func TestEndTrialGivesGraceThenReadOnly(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	trialEnded := now.Add(-time.Hour)
	g := &Gym{SubscriptionPlan: PlanTrial, SubscriptionStatus: StatusActive, TrialEndsAt: &trialEnded}

	g.EndTrial(now)

	if g.SubscriptionStatus != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", g.SubscriptionStatus)
	}
	wantGrace := now.Add(TrialGraceDays * 24 * time.Hour)
	if g.SubscriptionEndsAt == nil || !g.SubscriptionEndsAt.Equal(wantGrace) {
		t.Fatalf("grace ends = %v, want %v", g.SubscriptionEndsAt, wantGrace)
	}
	if g.IsTrialReadOnly(now) || g.IsAccessHardBlocked(now) {
		t.Fatal("during the grace the gym keeps full access")
	}
	after := wantGrace.Add(time.Minute)
	if !g.IsTrialReadOnly(after) {
		t.Fatal("after the grace the reception is read-only")
	}
	if g.IsAccessHardBlocked(after) {
		t.Fatal("an ended trial is never hard-blocked; it stays read-only with check-in")
	}
}

func TestEndTrialLongAgoStillGivesFullGrace(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	longAgo := now.Add(-60 * 24 * time.Hour)
	g := &Gym{SubscriptionPlan: PlanTrial, SubscriptionStatus: StatusActive, TrialEndsAt: &longAgo}
	g.EndTrial(now)
	if g.IsTrialReadOnly(now.Add(48 * time.Hour)) {
		t.Fatal("a trial closed late still gets its grace days of notice")
	}
}

func TestEndTrialBeforeItsEndKeepsRemainingDays(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	ends := now.Add(10 * 24 * time.Hour)
	g := &Gym{SubscriptionPlan: PlanTrial, SubscriptionStatus: StatusActive, TrialEndsAt: &ends}
	g.EndTrial(now)
	if g.IsTrialReadOnly(ends.Add(TrialGraceDays*24*time.Hour - time.Minute)) {
		t.Fatal("the trial days left plus the grace stay open")
	}
}

func TestReadOnlyLiftsWhenPlanActivatesOrTrialExtends(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	ended := now.Add(-10 * 24 * time.Hour)
	readOnly := func() *Gym {
		g := &Gym{SubscriptionPlan: PlanTrial, SubscriptionStatus: StatusActive, TrialEndsAt: &ended}
		g.EndTrial(now.Add(-5 * 24 * time.Hour))
		if !g.IsTrialReadOnly(now) {
			t.Fatal("setup: expected read-only")
		}
		return g
	}

	paid := readOnly()
	periodEnd := now.Add(30 * 24 * time.Hour)
	if err := paid.ActivateSubscription(PlanStandardMonthly, &periodEnd, now); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if paid.IsTrialReadOnly(now) || paid.IsAccessHardBlocked(now) {
		t.Fatal("paying lifts the read-only mode")
	}

	extended := readOnly()
	extended.ExtendTrial(7, now)
	if extended.IsTrialReadOnly(now) {
		t.Fatal("extending the trial lifts the read-only mode")
	}
}

func TestPaidPlanCancelledStillHardBlocks(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	ended := now.Add(-time.Hour)
	g := &Gym{SubscriptionPlan: PlanStandardMonthly, SubscriptionStatus: StatusCancelled, SubscriptionEndsAt: &ended}
	if !g.IsAccessHardBlocked(now) || g.IsTrialReadOnly(now) {
		t.Fatal("a cancelled paid plan keeps the full block")
	}
}
