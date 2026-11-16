package budget

import (
	"errors"
	"testing"
	"time"

	budgetErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
)

func configuredRules(t *testing.T) Rules {
	t.Helper()
	r, err := NewRules(map[Scope]Limit{Destination: {time.Minute, 2, 3}, Origin: {time.Minute, 2, 3}, Service: {time.Minute, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func subjectsFixture() map[Scope][32]byte {
	return map[Scope][32]byte{Destination: {1}, Origin: {2}, Service: {3}}
}

func TestBudgetRulesFailClosedWithoutCompletePositiveConfiguration(t *testing.T) {
	for _, limits := range []map[Scope]Limit{nil, {}, {Destination: {time.Minute, 2, 3}}, {Destination: {time.Minute, 2, 3}, Origin: {time.Minute, 2, 3}, Service: {time.Minute, 0, 3}}, {Destination: {time.Minute, 2, 3}, Origin: {time.Minute, 2, 0}, Service: {time.Minute, 2, 3}}, {Destination: {time.Millisecond, 2, 3}, Origin: {time.Minute, 2, 3}, Service: {time.Minute, 2, 3}}, {Destination: {time.Minute, 2, 3}, Origin: {time.Minute, 2, 3}, Recovery: {time.Minute, 2, 3}}} {
		if _, err := NewRules(limits); !errors.Is(err, budgetErrors.ErrBudgetConfiguration) {
			t.Fatalf("incomplete policy accepted: %v", err)
		}
	}
	if _, err := NewRecoveryRules(Limit{}); !errors.Is(err, budgetErrors.ErrBudgetConfiguration) {
		t.Fatal(err)
	}
	if (Plan{}).Valid() {
		t.Fatal("zero plan accepted")
	}
}

func TestBudgetPlanUsesUTCImmutableConfigurationAndNoChannels(t *testing.T) {
	limits := map[Scope]Limit{Destination: {time.Minute, 2, 3}, Origin: {time.Minute, 2, 3}, Service: {time.Minute, 2, 3}}
	r, err := NewRules(limits)
	if err != nil {
		t.Fatal(err)
	}
	limits[Destination] = Limit{}
	local := time.Date(2026, 10, 18, 23, 59, 59, 999999999, time.FixedZone("fixture", -6*3600))
	p, err := r.Plan(local, Send, subjectsFixture())
	if err != nil {
		t.Fatal(err)
	}
	if p.At().Location() != time.UTC || p.Action() != Send {
		t.Fatal("plan does not use server UTC")
	}
	dimensions := p.Dimensions()
	for i, want := range []Scope{Destination, Origin, Service} {
		if dimensions[i].Scope != want || dimensions[i].WindowStart != time.Date(2026, 10, 19, 5, 59, 0, 0, time.UTC) {
			t.Fatal("wrong dimension order or UTC window")
		}
	}
	dimensions[0].Limit.MaxSend = 0
	if p.Dimensions()[0].Limit.MaxSend != 2 {
		t.Fatal("plan aliased mutable caller data")
	}
	if _, err := r.Plan(local, Send, map[Scope][32]byte{Destination: {1}}); err == nil {
		t.Fatal("missing subjects accepted")
	}
	if _, err := r.Plan(local, Action("whatsapp"), subjectsFixture()); !errors.Is(err, budgetErrors.ErrBudgetInput) {
		t.Fatal("channel accepted as budget action")
	}
}

func TestBudgetCounterSeparatesSendCheckAndHonorsLimit(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 10, 0, time.UTC)
	c := Counter{Scope: Destination, SubjectHash: [32]byte{1}, WindowStart: WindowStart(now, time.Minute)}
	limit := Limit{time.Minute, 1, 2}
	if err := c.Reserve(now, Send, limit); err != nil {
		t.Fatal(err)
	}
	if err := c.Reserve(now, Send, limit); !errors.Is(err, budgetErrors.ErrBudgetExceeded) {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := c.Reserve(now, Check, limit); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Reserve(now, Check, limit); !errors.Is(err, budgetErrors.ErrBudgetExceeded) {
		t.Fatal(err)
	}
	if c.SendCount != 1 || c.CheckCount != 2 {
		t.Fatal("rejected reservation changed counts")
	}
}

func TestBudgetCounterBlockedUntilAndExactWindowBoundary(t *testing.T) {
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	until := start.Add(20 * time.Second)
	c := Counter{WindowStart: start, BlockedUntil: &until}
	limit := Limit{time.Minute, 1, 1}
	if err := c.Reserve(until.Add(-time.Nanosecond), Send, limit); !errors.Is(err, budgetErrors.ErrBudgetBlocked) {
		t.Fatal(err)
	}
	if err := c.Reserve(until, Send, limit); err != nil {
		t.Fatal(err)
	}
	if err := c.Reserve(start.Add(time.Minute), Send, limit); !errors.Is(err, budgetErrors.ErrBudgetInput) {
		t.Fatal("old row used for new window")
	}
	next := Counter{WindowStart: start.Add(time.Minute)}
	if err := next.Reserve(start.Add(time.Minute), Send, limit); err != nil {
		t.Fatal(err)
	}
	if !WindowStart(time.Unix(-1, 0), time.Minute).Equal(time.Unix(-60, 0)) {
		t.Fatal("negative epoch floor incorrect")
	}
	if _, err := configuredRules(t).Plan(time.Time{}, Send, subjectsFixture()); err == nil {
		t.Fatal("missing server clock accepted")
	}
}

func TestBudgetRecoveryHasSeparateConfiguredScope(t *testing.T) {
	r, err := NewRecoveryRules(Limit{time.Hour, 1, 3})
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.Plan(time.Now(), Check, map[Scope][32]byte{Recovery: {9}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Dimensions()) != 1 || p.Dimensions()[0].Scope != Recovery {
		t.Fatal("recovery shares phone limits")
	}
	if _, err := r.Plan(time.Now(), Check, map[Scope][32]byte{Destination: {9}}); err == nil {
		t.Fatal("recovery accepted a phone scope")
	}
}

func TestBudgetIntegerBoundaryAndCorruptCountsFailWithoutMutation(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 10, 0, time.UTC)
	limit := Limit{Window: time.Minute, MaxSend: 2147483647, MaxCheck: 2147483647}
	c := Counter{WindowStart: WindowStart(now, limit.Window), SendCount: 2147483646, CheckCount: 2147483646}
	for _, action := range []Action{Send, Check} {
		if err := c.Reserve(now, action, limit); err != nil {
			t.Fatal(err)
		}
		if err := c.Reserve(now, action, limit); !errors.Is(err, budgetErrors.ErrBudgetExceeded) {
			t.Fatal("integer upper bound was not enforced")
		}
	}
	if c.SendCount != 2147483647 || c.CheckCount != 2147483647 {
		t.Fatal("counter overflow")
	}
	for _, counts := range [][2]int{{-1, 0}, {0, -1}} {
		corrupt := Counter{WindowStart: c.WindowStart, SendCount: counts[0], CheckCount: counts[1]}
		if err := corrupt.Reserve(now, Send, limit); !errors.Is(err, budgetErrors.ErrBudgetCorrupt) || corrupt.SendCount != counts[0] || corrupt.CheckCount != counts[1] {
			t.Fatal("corrupt counter accepted or mutated")
		}
	}
}
