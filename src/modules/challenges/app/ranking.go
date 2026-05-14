package app

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	measurementDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/measurement"
	challengeRepo "github.com/cuadra/cuadra-core/src/modules/challenges/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/challenges/domain/scoring"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// rankingCacheTTL — short enough that the FE always sees fresh-ish numbers
// during a live event, long enough that a category screen refreshing every
// few seconds doesn't hammer the DB.
const rankingCacheTTL = 30 * time.Second

// RankingEntry is one row in the per-category ranking. Position is 1-based
// and assigned after sorting; Tied is true when this entry and the
// previous one are within the challenge's tie_margin_ir.
//
// AttendanceInsufficient is a flag, not a filter — the FE shows a warning
// badge but the participant still appears in their slot, because the
// disqualification decision is a separate workflow (CheckDisqualifications).
type RankingEntry struct {
	ParticipantID          uuid.UUID
	CategoryID             uuid.UUID
	MemberID               uuid.UUID
	IR                     float64
	Score                  scoring.ScoreBreakdown
	Position               int
	Tied                   bool
	AttendanceInsufficient bool
}

// GetChallengeRanking computes the ranking at query time from the active
// T₀/T₁ measurements. Result is cached per challenge_id for rankingCacheTTL.
type GetChallengeRanking struct {
	Challenges   challengeRepo.ChallengeRepository
	Participants challengeRepo.ParticipantRepository
	Measurements challengeRepo.MeasurementRepository
	Attendance   challengeRepo.AttendanceCounter
	UoW          sharedDomain.UnitOfWork
	NowFunc      func() time.Time

	cache sync.Map // map[uuid.UUID]rankingCacheEntry
}

type rankingCacheEntry struct {
	at      time.Time
	entries []RankingEntry
}

func NewGetChallengeRanking(
	challenges challengeRepo.ChallengeRepository,
	participants challengeRepo.ParticipantRepository,
	measurements challengeRepo.MeasurementRepository,
	attendance challengeRepo.AttendanceCounter,
	uow sharedDomain.UnitOfWork,
) *GetChallengeRanking {
	return &GetChallengeRanking{
		Challenges: challenges, Participants: participants, Measurements: measurements,
		Attendance: attendance, UoW: uow,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type GetChallengeRankingInput struct {
	GymID       uuid.UUID
	ChallengeID uuid.UUID
	CategoryID  *uuid.UUID // optional filter
}

func (uc *GetChallengeRanking) Execute(ctx context.Context, in GetChallengeRankingInput) ([]RankingEntry, error) {
	now := uc.NowFunc()

	// Cache lookup — keyed on challenge only. Category filtering happens
	// after the cached compute (cheap slice filter).
	if v, ok := uc.cache.Load(in.ChallengeID); ok {
		if e := v.(rankingCacheEntry); now.Sub(e.at) < rankingCacheTTL {
			return filterByCategory(e.entries, in.CategoryID), nil
		}
	}

	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	ch, err := uc.Challenges.GetByID(tx, in.ChallengeID)
	if err != nil {
		return nil, err
	}
	if ch.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
	}

	parts, err := uc.Participants.ListByChallenge(tx, ch.ID, "", nil)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	// Group participants by category so we can rank inside each bucket.
	byCategory := map[uuid.UUID][]RankingEntry{}
	for _, p := range parts {
		t0m, hasT0, err := uc.Measurements.GetActiveByMoment(tx, p.ID, measurementDomain.MomentT0)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		t1m, hasT1, err := uc.Measurements.GetActiveByMoment(tx, p.ID, measurementDomain.MomentT1)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		// Both T₀ and T₁ are required to score. Missing one drops the
		// participant from the ranking entirely (the FE may show them in
		// a "sin medición" tab if it wants).
		if !hasT0 || !hasT1 {
			continue
		}
		score := scoring.CalculateIR(t0m.ToScoringInput(), t1m.ToScoringInput(), ch.StrengthCapPct)
		insufficient := attendanceBelowMinimum(tx, uc.Attendance, ch, p.GymID, p.MemberID, now)
		byCategory[p.CategoryID] = append(byCategory[p.CategoryID], RankingEntry{
			ParticipantID:          p.ID,
			CategoryID:             p.CategoryID,
			MemberID:               p.MemberID,
			IR:                     score.IR,
			Score:                  score,
			AttendanceInsufficient: insufficient,
		})
	}

	var all []RankingEntry
	for catID, entries := range byCategory {
		_ = catID
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].IR > entries[j].IR })
		// Assign positions + tie flag in the same pass. Tied means
		// |IR_i - IR_{i-1}| <= TieMarginIR (both rows get the flag).
		for i := range entries {
			entries[i].Position = i + 1
			if i > 0 && math.Abs(entries[i].IR-entries[i-1].IR) <= ch.TieMarginIR {
				entries[i].Tied = true
				entries[i-1].Tied = true
			}
		}
		all = append(all, entries...)
	}

	uc.cache.Store(in.ChallengeID, rankingCacheEntry{at: now, entries: all})
	return filterByCategory(all, in.CategoryID), nil
}

// InvalidateCache lets the use case clear a single challenge's entry after
// a write (capture, supersession, transition). Today wired only by tests;
// in production the 30s TTL is good enough.
func (uc *GetChallengeRanking) InvalidateCache(challengeID uuid.UUID) {
	uc.cache.Delete(challengeID)
}

func filterByCategory(entries []RankingEntry, catID *uuid.UUID) []RankingEntry {
	if catID == nil {
		return entries
	}
	out := make([]RankingEntry, 0, len(entries))
	for _, e := range entries {
		if e.CategoryID == *catID {
			out = append(out, e)
		}
	}
	return out
}

// attendanceBelowMinimum returns true when the participant logged fewer
// check-ins than min_weekly_attendance × weeks-elapsed allows, even after
// the grace weeks credit. Errors from the counter are absorbed — we'd
// rather show a participant unflagged than fail the whole ranking.
func attendanceBelowMinimum(tx sharedDomain.Transaction, counter challengeRepo.AttendanceCounter, ch *challengeDomain.Challenge, gymID, memberID uuid.UUID, now time.Time) bool {
	if counter == nil {
		return false
	}
	from := ch.StartsAt
	to := now
	if to.After(ch.EndsAt) {
		to = ch.EndsAt
	}
	if !to.After(from) {
		return false
	}
	weeks := int(math.Ceil(to.Sub(from).Hours() / (24.0 * 7.0)))
	if weeks <= ch.AttendanceGraceWeeks {
		return false
	}
	required := (weeks - ch.AttendanceGraceWeeks) * ch.MinWeeklyAttendance
	if required <= 0 {
		return false
	}
	count, err := counter.CountInRange(tx, gymID, memberID, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return false
	}
	return count < required
}
