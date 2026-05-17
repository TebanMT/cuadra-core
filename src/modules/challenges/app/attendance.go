package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	participantDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/participant"
	challengeRepo "github.com/cuadra/cuadra-core/src/modules/challenges/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// WeeklyAttendance is a per-week roll-up for a single participant. Met is
// true when the count meets `min_weekly_attendance`. WeekStart/WeekEnd are
// in UTC; the FE converts to gym timezone for display.
type WeeklyAttendance struct {
	WeekIndex int // 0-based; week 0 starts at challenge.starts_at
	WeekStart time.Time
	WeekEnd   time.Time
	Count     int
	Met       bool
}

// AttendanceReport is the per-participant slice the dashboard shows.
type AttendanceReport struct {
	ParticipantID     uuid.UUID
	MemberID          uuid.UUID
	CategoryID        uuid.UUID
	Weeks             []WeeklyAttendance
	WeeksMet          int
	WeeksMissed       int
	GraceWeeksAllowed int
	WithinGrace       bool
	Status            string
}

// GetAttendanceReport returns the week-by-week attendance for every
// participant in the challenge. Walks the time window from starts_at to
// min(now, ends_at), bucketed in 7-day steps.
type GetAttendanceReport struct {
	Challenges   challengeRepo.ChallengeRepository
	Participants challengeRepo.ParticipantRepository
	Attendance   challengeRepo.AttendanceCounter
	UoW          sharedDomain.UnitOfWork
	NowFunc      func() time.Time
}

func NewGetAttendanceReport(
	challenges challengeRepo.ChallengeRepository,
	participants challengeRepo.ParticipantRepository,
	attendance challengeRepo.AttendanceCounter,
	uow sharedDomain.UnitOfWork,
) *GetAttendanceReport {
	return &GetAttendanceReport{
		Challenges: challenges, Participants: participants, Attendance: attendance,
		UoW:     uow,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type GetAttendanceReportInput struct {
	GymID       uuid.UUID
	ChallengeID uuid.UUID
}

func (uc *GetAttendanceReport) Execute(ctx context.Context, in GetAttendanceReportInput) ([]AttendanceReport, error) {
	now := uc.NowFunc()
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

	reportEnd := now
	if reportEnd.After(ch.EndsAt) {
		reportEnd = ch.EndsAt
	}

	out := make([]AttendanceReport, 0, len(parts))
	for _, p := range parts {
		weeks := buildWeeks(ch, p.GymID, p.MemberID, uc.Attendance, tx, reportEnd)
		met, missed := 0, 0
		for _, w := range weeks {
			if w.Met {
				met++
			} else {
				missed++
			}
		}
		out = append(out, AttendanceReport{
			ParticipantID:     p.ID,
			MemberID:          p.MemberID,
			CategoryID:        p.CategoryID,
			Weeks:             weeks,
			WeeksMet:          met,
			WeeksMissed:       missed,
			GraceWeeksAllowed: ch.AttendanceGraceWeeks,
			WithinGrace:       missed <= ch.AttendanceGraceWeeks,
			Status:            p.Status,
		})
	}
	return out, nil
}

func buildWeeks(ch *challengeDomain.Challenge, gymID, memberID uuid.UUID, counter challengeRepo.AttendanceCounter, tx sharedDomain.Transaction, reportEnd time.Time) []WeeklyAttendance {
	const week = 7 * 24 * time.Hour
	var weeks []WeeklyAttendance
	i := 0
	for start := ch.StartsAt; start.Before(reportEnd); start = start.Add(week) {
		end := start.Add(week)
		if end.After(reportEnd) {
			end = reportEnd
		}
		count := 0
		if counter != nil {
			if n, err := counter.CountInRange(tx, gymID, memberID, start.UnixMilli(), end.UnixMilli()); err == nil {
				count = n
			}
		}
		weeks = append(weeks, WeeklyAttendance{
			WeekIndex: i,
			WeekStart: start,
			WeekEnd:   end,
			Count:     count,
			Met:       count >= ch.MinWeeklyAttendance,
		})
		i++
	}
	return weeks
}

// ─── CheckDisqualifications ────────────────────────────────────────────────

// CheckDisqualifications walks every active participant and disqualifies
// the ones whose missed-week count exceeds attendance_grace_weeks. Safe to
// run idempotently — Disqualify() is a no-op once status is already
// `disqualified`.
type CheckDisqualifications struct {
	Challenges   challengeRepo.ChallengeRepository
	Participants challengeRepo.ParticipantRepository
	Attendance   challengeRepo.AttendanceCounter
	UoW          sharedDomain.UnitOfWork
	Audit        audit.Recorder
	NowFunc      func() time.Time
}

func NewCheckDisqualifications(
	challenges challengeRepo.ChallengeRepository,
	participants challengeRepo.ParticipantRepository,
	attendance challengeRepo.AttendanceCounter,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *CheckDisqualifications {
	return &CheckDisqualifications{
		Challenges: challenges, Participants: participants, Attendance: attendance,
		UoW: uow, Audit: recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type CheckDisqualificationsInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	ChallengeID uuid.UUID
}

type CheckDisqualificationsOutput struct {
	DisqualifiedIDs []uuid.UUID
}

func (uc *CheckDisqualifications) Execute(ctx context.Context, in CheckDisqualificationsInput) (*CheckDisqualificationsOutput, error) {
	now := uc.NowFunc()
	out := &CheckDisqualificationsOutput{}
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		ch, err := uc.Challenges.GetByID(tx, in.ChallengeID)
		if err != nil {
			return err
		}
		if ch.GymID != in.GymID {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
		}
		parts, err := uc.Participants.ListByChallenge(tx, ch.ID, participantDomain.StatusActive, nil)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		reportEnd := now
		if reportEnd.After(ch.EndsAt) {
			reportEnd = ch.EndsAt
		}
		for _, p := range parts {
			weeks := buildWeeks(ch, p.GymID, p.MemberID, uc.Attendance, tx, reportEnd)
			missed := 0
			for _, w := range weeks {
				if !w.Met {
					missed++
				}
			}
			if missed <= ch.AttendanceGraceWeeks {
				continue
			}
			p.Disqualify("asistencia insuficiente", now)
			if _, err := uc.Participants.Update(tx, p); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			out.DisqualifiedIDs = append(out.DisqualifiedIDs, p.ID)
			_ = uc.Audit.Record(ctx, tx, audit.Entry{
				GymID:       in.GymID,
				EntityType:  "challenge_participants",
				EntityID:    p.ID,
				Action:      audit.ActionUpdate,
				ActorUserID: &in.ActorUserID,
				Changes: map[string]any{
					"status": participantDomain.StatusDisqualified,
					"reason": "asistencia insuficiente",
				},
				IPAddress: audit.IPFromContext(ctx),
				UserAgent: audit.UAFromContext(ctx),
				At:        now,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
