package workout

import (
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
	"math"
	"strconv"
	"time"
)

const MaxSetIDs = 1000

type Set struct {
	ID, AccountID, WorkoutID, WorkoutExerciseID, OriginDeviceID uuid.UUID
	PositionHint, Repetitions                                   int
	Load                                                        Load
	Revision                                                    int64
	CapturedAt, CreatedAt, UpdatedAt                            time.Time
	DeletedAt                                                   *time.Time
}

func (s *Set) Validate() error {
	if s == nil {
		return trainingErrors.ErrInvalidWorkoutSet
	}
	for _, id := range []uuid.UUID{s.ID, s.AccountID, s.WorkoutID, s.WorkoutExerciseID, s.OriginDeviceID} {
		if !protocol.ValidID(id) {
			return trainingErrors.ErrInvalidWorkoutSet
		}
	}
	if s.ID == s.WorkoutID || s.ID == s.WorkoutExerciseID || s.PositionHint < 0 || s.PositionHint > 9999 || s.Repetitions < 0 || s.Repetitions > 10000 || s.Revision < 1 || !validTime(s.CapturedAt) || !validTime(s.CreatedAt) || !validTime(s.UpdatedAt) || s.UpdatedAt.Before(s.CreatedAt) || s.DeletedAt != nil && !validTime(*s.DeletedAt) {
		return trainingErrors.ErrInvalidWorkoutSet
	}
	canonical, err := CanonicalLoad(s.Load.LoadMode, s.Load.WeightValue, s.Load.WeightUnit)
	if err != nil || canonical.WeightValue != nil && *canonical.WeightValue != *s.Load.WeightValue {
		return trainingErrors.ErrInvalidWorkoutSet
	}
	return nil
}
func (s *Set) ValidateCreation(owner uuid.UUID) error {
	if s.Validate() != nil || s.AccountID != owner || s.Revision != 1 || s.DeletedAt != nil || !s.CreatedAt.Equal(s.UpdatedAt) {
		return trainingErrors.ErrInvalidWorkoutSet
	}
	return nil
}
func (w *Workout) AllowsLoad(appearance uuid.UUID, load Load) bool {
	if w.Validate() != nil {
		return false
	}
	for _, e := range w.Exercises {
		if e.ID == appearance {
			return e.LoadModeSnapshot == load.LoadMode
		}
	}
	return false
}

// NewSet never mutates its input aggregate or normalizes request/hash material.
// The caller resolves observed revision and count before applying this effect.
func NewSet(w *Workout, id, origin, appearance uuid.UUID, position, repetitions int, load Load, captured, now time.Time) (*Set, error) {
	if !w.AllowsLoad(appearance, load) {
		return nil, trainingErrors.ErrInvalidWorkoutSet
	}
	canonical, err := CanonicalLoad(load.LoadMode, load.WeightValue, load.WeightUnit)
	if err != nil {
		return nil, trainingErrors.ErrInvalidWorkoutSet
	}
	s := &Set{ID: id, AccountID: w.AccountID, WorkoutID: w.ID, WorkoutExerciseID: appearance, OriginDeviceID: origin, PositionHint: position, Repetitions: repetitions, Load: canonical, Revision: 1, CapturedAt: captured.UTC(), CreatedAt: now.UTC().Truncate(time.Millisecond), UpdatedAt: now.UTC().Truncate(time.Millisecond)}
	if s.ValidateCreation(w.AccountID) != nil {
		return nil, trainingErrors.ErrInvalidWorkoutSet
	}
	return s, nil
}

// WithNewSet changes only revision/update/review metadata. Finalization and the
// immutable prescription remain intact, even for a late independent series.
func (w *Workout) WithNewSet(s *Set, now time.Time) (*Workout, error) {
	if w.Validate() != nil || s.ValidateCreation(w.AccountID) != nil || s.WorkoutID != w.ID || !w.AllowsLoad(s.WorkoutExerciseID, s.Load) || w.DeletedAt != nil {
		return nil, trainingErrors.ErrInvalidWorkoutSet
	}
	if w.Revision == math.MaxInt64 {
		return nil, trainingErrors.ErrTrainingLimit
	}
	out := *w
	out.Exercises = append([]Exercise(nil), w.Exercises...)
	for i := range out.Exercises {
		out.Exercises[i].RestSeconds = copyInt(out.Exercises[i].RestSeconds)
	}
	if w.FinishedAt != nil {
		at := *w.FinishedAt
		out.FinishedAt = &at
	}
	if w.FinalizedRevision != nil {
		n := *w.FinalizedRevision
		out.FinalizedRevision = &n
	}
	out.Revision++
	out.UpdatedAt = now.UTC().Truncate(time.Millisecond)
	out.NeedsReview = w.NeedsReview || w.State == "completed"
	if out.Validate() != nil {
		return nil, trainingErrors.ErrInvalidWorkoutSet
	}
	return &out, nil
}

type SetDTO struct {
	ID                string    `json:"id"`
	Revision          string    `json:"revision"`
	CreatedAt         string    `json:"created_at"`
	UpdatedAt         string    `json:"updated_at"`
	DeletedAt         *string   `json:"deleted_at"`
	WorkoutID         string    `json:"workout_id"`
	WorkoutExerciseID string    `json:"workout_exercise_id"`
	OriginDeviceID    string    `json:"origin_device_id"`
	PositionHint      int       `json:"position_hint"`
	Load              LoadInput `json:"load"`
	Repetitions       int       `json:"repetitions"`
	CapturedAt        string    `json:"captured_at"`
}

func (s *Set) DTO() SetDTO {
	load, _ := CanonicalLoad(s.Load.LoadMode, s.Load.WeightValue, s.Load.WeightUnit)
	out := SetDTO{ID: s.ID.String(), Revision: strconv.FormatInt(s.Revision, 10), CreatedAt: protocol.Timestamp(s.CreatedAt), UpdatedAt: protocol.Timestamp(s.UpdatedAt), WorkoutID: s.WorkoutID.String(), WorkoutExerciseID: s.WorkoutExerciseID.String(), OriginDeviceID: s.OriginDeviceID.String(), PositionHint: s.PositionHint, Load: LoadInput{LoadMode: load.LoadMode, WeightValue: load.WeightValue, WeightUnit: load.WeightUnit}, Repetitions: s.Repetitions, CapturedAt: protocol.Timestamp(s.CapturedAt)}
	if s.DeletedAt != nil {
		at := protocol.Timestamp(*s.DeletedAt)
		out.DeletedAt = &at
	}
	return out
}
