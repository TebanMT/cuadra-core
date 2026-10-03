package cashclose

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSessionOpeningActivityCountAndWithdrawal(t *testing.T) {
	gym, actor := uuid.New(), uuid.New()
	now := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	s, err := Open(gym, uuid.Nil, actor, now, 1, 200, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(500, actor, now.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s.CalculatedCash != 700 || s.Status != StatusClosedUnverified {
		t.Fatalf("close = status %s expected %.2f", s.Status, s.CalculatedCash)
	}
	if err := s.Reconcile(700, nil, actor, now.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if d := s.Difference(); d == nil || *d != 0 {
		t.Fatalf("difference = %v, want 0", d)
	}
	if err := s.Withdraw(200, DestinationGymFund, actor, now.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusWithdrawn || s.WithdrawnCash == nil || *s.WithdrawnCash != 500 {
		t.Fatalf("withdraw = status %s amount %v", s.Status, s.WithdrawnCash)
	}
	transfer := NewTransfer(s)
	if transfer == nil || transfer.Amount != 500 || transfer.Destination != DestinationGymFund {
		t.Fatalf("transfer = %+v", transfer)
	}
}

func TestSessionStaleSuppressesDifferenceAndCanRefresh(t *testing.T) {
	gym, actor := uuid.New(), uuid.New()
	now := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	s, _ := Open(gym, uuid.Nil, actor, now, 1, 0, now)
	_ = s.Close(100, actor, now.Add(time.Hour))
	_ = s.Reconcile(100, nil, actor, now.Add(time.Hour))
	if err := s.MarkStale("ingreso fuera de horario", now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s.Difference() != nil || s.EffectiveStatus(false) != StatusStale {
		t.Fatalf("stale must not expose variance: %+v", s.Difference())
	}
	if err := s.Refresh(150, "ingreso fuera de horario", actor, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusClosedUnverified || s.CalculatedCash != 150 || s.CountedCash != nil {
		t.Fatalf("refreshed = %+v", s)
	}
}

func TestSessionValidatesCentsAndDifferenceReason(t *testing.T) {
	gym, actor := uuid.New(), uuid.New()
	now := time.Now().UTC()
	if _, err := Open(gym, uuid.Nil, actor, now, 1, 10.001, now); !errors.Is(err, ErrInvalidMoney) {
		t.Fatalf("opening err = %v", err)
	}
	s, _ := Open(gym, uuid.Nil, actor, now, 1, 0, now)
	_ = s.Close(100, actor, now)
	if err := s.Reconcile(99, nil, actor, now); !errors.Is(err, ErrDifferenceReason) {
		t.Fatalf("difference err = %v", err)
	}
	reason := "faltó un peso"
	if err := s.Reconcile(99, &reason, actor, now); err != nil {
		t.Fatal(err)
	}
}

func TestDeterministicSessionIDIncludesDrawerDateAndSequence(t *testing.T) {
	gym, drawer := uuid.New(), uuid.New()
	date := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	a := DeterministicSessionID(gym, drawer, date, 1)
	if a != DeterministicSessionID(gym, drawer, date, 1) {
		t.Fatal("same natural key must converge")
	}
	if a == DeterministicSessionID(gym, drawer, date, 2) || a == DeterministicSessionID(gym, uuid.New(), date, 1) {
		t.Fatal("sequence and drawer must be part of identity")
	}
}
