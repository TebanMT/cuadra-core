package cashclose

import (
	"errors"
	"strings"
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

func TestReviewWithoutCountMarksOnlyClosedSessionsAndLapsesOnReclose(t *testing.T) {
	gym, actor := uuid.New(), uuid.New()
	start := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	s, err := Open(gym, uuid.Nil, actor, start, 1, 0, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReviewWithoutCount("se olvidó el corte", actor, start); !errors.Is(err, ErrInvalidSessionState) {
		t.Fatalf("open session review err = %v, want ErrInvalidSessionState", err)
	}
	closedAt := start.Add(10 * time.Hour)
	if err := s.Close(300, actor, closedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ReviewWithoutCount(" \n\t ", actor, closedAt); !errors.Is(err, ErrReviewNote) {
		t.Fatalf("blank note err = %v, want ErrReviewNote", err)
	}
	long := make([]rune, 501)
	for i := range long {
		long[i] = 'ñ'
	}
	if err := s.ReviewWithoutCount(string(long), actor, closedAt); !errors.Is(err, ErrReviewNoteTooLong) {
		t.Fatalf("long note err = %v, want ErrReviewNoteTooLong", err)
	}
	// A device clock behind closed_at must not produce an already-lapsed review.
	if err := s.ReviewWithoutCount("  nadie  contó\nla caja ", actor, closedAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !s.ReviewedWithoutCount() || s.ReviewNote == nil || *s.ReviewNote != "nadie contó la caja" ||
		s.ReviewedBy == nil || *s.ReviewedBy != actor || !s.ReviewedAt.Equal(closedAt) {
		t.Fatalf("review = at %v by %v note %v", s.ReviewedAt, s.ReviewedBy, s.ReviewNote)
	}
	if s.Status != StatusClosedUnverified || s.Difference() != nil {
		t.Fatalf("review must not certify: status %s difference %v", s.Status, s.Difference())
	}

	// A peer that predates the review columns re-closes without clearing them.
	stale := *s
	stale.ClosedAt = ptrTime(closedAt.Add(time.Hour))
	if stale.ReviewedWithoutCount() {
		t.Fatal("review must lapse once the session is re-closed after it")
	}
	if err := s.MarkStale("pago tardío", closedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s.ReviewedAt != nil || s.ReviewNote != nil || s.ReviewedWithoutCount() {
		t.Fatalf("stale session kept review: %v %v", s.ReviewedAt, s.ReviewNote)
	}
}

func ptrTime(v time.Time) *time.Time { return &v }

func TestCheckOpeningAcceptsADifferentOpeningOnlyWithANote(t *testing.T) {
	now := time.Now().UTC()
	open := func(opening float64) *CashCloseEvent {
		t.Helper()
		e, err := Open(uuid.New(), uuid.Nil, uuid.New(), now, 1, opening, now)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	same := open(500)
	if err := same.CheckOpening(500, "  "); err != nil || same.OpeningDifference() != nil || same.OpeningNote != nil ||
		same.OpeningExpectedCash == nil || *same.OpeningExpectedCash != 500 {
		t.Fatalf("matching opening err=%v %+v", err, same)
	}
	// A recount that ends matching keeps its trace, without becoming a difference.
	recounted := open(500)
	if err := recounted.CheckOpening(500, " Primer conteo:  $450.00. "); err != nil || recounted.OpeningDifference() != nil ||
		recounted.OpeningNote == nil || *recounted.OpeningNote != "Primer conteo: $450.00." {
		t.Fatalf("recounted matching opening err=%v note=%v", err, recounted.OpeningNote)
	}
	if err := open(500).CheckOpening(500, strings.Repeat("a", 501)); !errors.Is(err, ErrOpeningNoteTooLong) {
		t.Fatalf("long note on a matching opening = %v", err)
	}
	short := open(450)
	if err := short.CheckOpening(500, " "); !errors.Is(err, ErrOpeningNote) {
		t.Fatalf("short without note = %v", err)
	}
	if err := short.CheckOpening(500, strings.Repeat("a", 501)); !errors.Is(err, ErrOpeningNoteTooLong) {
		t.Fatalf("long note = %v", err)
	}
	if err := short.CheckOpening(500, "Faltaban 50"); err != nil {
		t.Fatal(err)
	}
	if d := short.OpeningDifference(); d == nil || *d != -50 || short.OpeningNote == nil || *short.OpeningNote != "Faltaban 50" {
		t.Fatalf("short opening diff=%v note=%v", d, short.OpeningNote)
	}
	if err := open(10).CheckOpening(-1, "x"); err == nil {
		t.Fatal("a negative reference is not trustworthy")
	}
}
