package app

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/checkins/domain/checkin"
	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	chkRepo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// NumberAttemptLimiter is an anti-enumeration guard for check-in-by-number
// (ADR-010 §2.4). Since the member number is PUBLIC (no longer a secret), the
// limiter's job is no longer brute-force-of-a-secret defense — it's just to
// stop a script from sweeping the number space at the kiosko. Hence the
// relaxed thresholds vs. the old PIN limiter (5/60s): a real member mistyping
// won't trip it, but a barrido sostenido pausa. Lives in-memory only (resets
// on sidecar restart), per-gym.
type NumberAttemptLimiter struct {
	mu       sync.Mutex
	windows  map[uuid.UUID]*numberWindow
	max      int
	window   time.Duration
	cooldown time.Duration
}

type numberWindow struct {
	failures  []time.Time
	blockedTo time.Time
}

// NewNumberAttemptLimiter — defaults relajados para anti-enumeración:
// 10 fallos en 60s ⇒ 60s de pausa. Un socio que se equivoca un par de veces
// nunca lo dispara; un barrido automatizado sí.
func NewNumberAttemptLimiter() *NumberAttemptLimiter {
	return &NumberAttemptLimiter{
		windows:  make(map[uuid.UUID]*numberWindow),
		max:      10,
		window:   60 * time.Second,
		cooldown: 60 * time.Second,
	}
}

// CheckinByNumberInput backs UC-032 step 4 (the lookup phase) — ADR-010.
type CheckinByNumberInput struct {
	GymID  uuid.UUID
	Number int
	Now    time.Time
}

// CheckinByNumber resolves a member by their public number with an O(1) lookup
// on `uq_members_gym_number`; on a hit, runs the same access evaluation as
// fingerprint/manual and records the checkin. Replaces the old O(N) bcrypt
// scan + first-match bug (ADR-010 §2.4): the unique index makes multi-match
// impossible and the lookup is a single indexed read.
type CheckinByNumber struct {
	Members *memApp.MemberService
	Finder  memRepo.MemberNumberFinder // member repo cast to its optional capability
	Repo    chkRepo.CheckinRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
	Limiter *NumberAttemptLimiter
	// Gyms (opcional) resuelve la zona horaria del gym para evaluar el acceso
	// en el día LOCAL, no UTC. Sin él, cae al día UTC (legacy). WithGyms.
	Gyms gymRepo.GymRepository
}

// WithGyms cablea el repo de gyms para que el check-in evalúe la vigencia en
// la zona horaria del gym (ver gymLocalToday). Fluent para main.go.
func (uc *CheckinByNumber) WithGyms(g gymRepo.GymRepository) *CheckinByNumber {
	uc.Gyms = g
	return uc
}

func NewCheckinByNumber(
	members *memApp.MemberService,
	finder memRepo.MemberNumberFinder,
	repo chkRepo.CheckinRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
	limiter *NumberAttemptLimiter,
) *CheckinByNumber {
	if limiter == nil {
		limiter = NewNumberAttemptLimiter()
	}
	return &CheckinByNumber{Members: members, Finder: finder, Repo: repo, UoW: uow, Audit: recorder, Limiter: limiter}
}

func (uc *CheckinByNumber) Execute(ctx context.Context, in CheckinByNumberInput) (*CheckinView, error) {
	if in.Number <= 0 {
		return nil, sharedDomain.NewValidationError(chkErrors.ErrNumberFormat)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if uc.Limiter.IsBlocked(in.GymID, now) {
		return nil, sharedDomain.NewBusinessError(chkErrors.ErrNumberTooManyAttempts, "")
	}

	var view *CheckinView
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		m, err := uc.Finder.FindByMemberNumber(tx, in.GymID, in.Number)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if m == nil {
			uc.Limiter.RecordFailure(in.GymID, now)
			return sharedDomain.NewBusinessError(chkErrors.ErrNumberIncorrect, "")
		}
		uc.Limiter.RecordSuccess(in.GymID)

		// Método "number" (ADR-010): check-in por número de socio.
		v, err := recordCheckin(ctx, tx, uc.Members, uc.Gyms, uc.Repo, uc.Audit,
			in.GymID, m.ID, checkin.MethodNumber, nil, now)
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
// NumberAttemptLimiter
// ---------------------------------------------------------------------------

// IsBlocked reports whether the gym is currently in cooldown.
func (l *NumberAttemptLimiter) IsBlocked(gymID uuid.UUID, now time.Time) bool {
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
func (l *NumberAttemptLimiter) RecordFailure(gymID uuid.UUID, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.windows[gymID]
	if !ok {
		w = &numberWindow{}
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

// RecordSuccess clears the gym's failure window — a successful entry resets
// the lockout counter.
func (l *NumberAttemptLimiter) RecordSuccess(gymID uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w, ok := l.windows[gymID]; ok {
		w.failures = nil
		w.blockedTo = time.Time{}
	}
}
