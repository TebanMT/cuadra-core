package app

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	chkRepo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// PinAttemptLimiter provides DA-32 anti-bruteforce: 5 failed attempts in 60s
// per gym block PIN entry. Lives in-memory only (resets on sidecar restart),
// which is intentional — sustained brute force across restarts is implausible
// against a 4-digit space (10⁴ codes) when each attempt requires physical
// access to the kiosko.
type PinAttemptLimiter struct {
	mu       sync.Mutex
	windows  map[uuid.UUID]*pinWindow
	max      int
	window   time.Duration
	cooldown time.Duration
}

type pinWindow struct {
	failures  []time.Time
	blockedTo time.Time
}

// NewPinAttemptLimiter — defaults: 5 failures in 60s ⇒ 60s cooldown.
func NewPinAttemptLimiter() *PinAttemptLimiter {
	return &PinAttemptLimiter{
		windows:  make(map[uuid.UUID]*pinWindow),
		max:      5,
		window:   60 * time.Second,
		cooldown: 60 * time.Second,
	}
}

// CheckinByPinInput backs UC-032 step 4 (the lookup phase).
type CheckinByPinInput struct {
	GymID uuid.UUID
	Pin   string
	Now   time.Time
}

// CheckinByPin matches a 4-digit PIN against active members in the gym; on a
// hit, runs the same access evaluation as fingerprint/manual and records the
// checkin. The lookup is O(N_pins) bcrypt verifies — same cost model as
// AssignPin.
type CheckinByPin struct {
	Members   *memApp.MemberService
	PinFinder memRepo.MemberPinCandidateLister // member repo cast to its optional capability
	Repo      chkRepo.CheckinRepository
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
	Limiter   *PinAttemptLimiter
}

func NewCheckinByPin(
	members *memApp.MemberService,
	pinFinder memRepo.MemberPinCandidateLister,
	repo chkRepo.CheckinRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
	limiter *PinAttemptLimiter,
) *CheckinByPin {
	if limiter == nil {
		limiter = NewPinAttemptLimiter()
	}
	return &CheckinByPin{Members: members, PinFinder: pinFinder, Repo: repo, UoW: uow, Audit: recorder, Limiter: limiter}
}

func (uc *CheckinByPin) Execute(ctx context.Context, in CheckinByPinInput) (*CheckinView, error) {
	if len(in.Pin) != 4 {
		return nil, sharedDomain.NewValidationError(chkErrors.ErrPinFormat)
	}
	for _, c := range in.Pin {
		if c < '0' || c > '9' {
			return nil, sharedDomain.NewValidationError(chkErrors.ErrPinFormat)
		}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if uc.Limiter.IsBlocked(in.GymID, now) {
		return nil, sharedDomain.NewBusinessError(chkErrors.ErrPinTooManyAttempts, "")
	}

	today := truncateToDay(now)

	var view *CheckinView
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		cands, err := uc.PinFinder.ListPinCandidates(tx, in.GymID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		var matchedID uuid.UUID
		for _, c := range cands {
			if bcrypt.CompareHashAndPassword([]byte(c.PinHash), []byte(in.Pin)) == nil {
				matchedID = c.MemberID
				break
			}
		}
		if matchedID == uuid.Nil {
			uc.Limiter.RecordFailure(in.GymID, now)
			return sharedDomain.NewBusinessError(chkErrors.ErrPinIncorrect, "")
		}
		uc.Limiter.RecordSuccess(in.GymID)

		v, err := recordCheckin(ctx, tx, uc.Members, uc.Repo, uc.Audit,
			in.GymID, matchedID, "pin", nil, now, today)
		if err != nil {
			return err
		}
		view = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}

// ---------------------------------------------------------------------------
// PinAttemptLimiter
// ---------------------------------------------------------------------------

// IsBlocked reports whether the gym is currently in cooldown.
func (l *PinAttemptLimiter) IsBlocked(gymID uuid.UUID, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.windows[gymID]
	if !ok {
		return false
	}
	return !w.blockedTo.IsZero() && now.Before(w.blockedTo)
}

// RecordFailure increments the gym's failure window. When max is reached we
// set blockedTo and clear the window so a fresh count starts after cooldown.
func (l *PinAttemptLimiter) RecordFailure(gymID uuid.UUID, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.windows[gymID]
	if !ok {
		w = &pinWindow{}
		l.windows[gymID] = w
	}
	cutoff := now.Add(-l.window)
	pruned := w.failures[:0]
	for _, t := range w.failures {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	pruned = append(pruned, now)
	w.failures = pruned
	if len(w.failures) >= l.max {
		w.blockedTo = now.Add(l.cooldown)
		w.failures = nil
	}
}

// RecordSuccess clears the gym's failure window — a successful PIN entry
// resets the lockout counter.
func (l *PinAttemptLimiter) RecordSuccess(gymID uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w, ok := l.windows[gymID]; ok {
		w.failures = nil
		w.blockedTo = time.Time{}
	}
}
