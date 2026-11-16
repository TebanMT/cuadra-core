package workout

import (
	"crypto/subtle"
	"encoding/hex"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Exercise struct {
	ID, SourceExerciseID           uuid.UUID
	Reference                      routine.ExerciseRef
	NameSnapshot, LoadModeSnapshot string
	Position, PrescribedSets       int
	PrescribedReps                 string
	RestSeconds                    *int
	Notes                          string
}
type Workout struct {
	ID, AccountID, RoutineVersionID, RoutineDayID, OriginDeviceID uuid.UUID
	RoutineTitleSnapshot, DayLabelSnapshot, State, Timezone       string
	StartedAt, CreatedAt, UpdatedAt                               time.Time
	FinishedAt, DeletedAt                                         *time.Time
	Revision                                                      int64
	FinalizedRevision                                             *int64
	NeedsReview                                                   bool
	Exercises                                                     []Exercise
}

func validText(s string, min, max int) bool {
	n := utf8.RuneCountInString(s)
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0) && n >= min && n <= max
}
func requiredText(s string, max int) bool { return validText(s, 1, max) && strings.TrimSpace(s) != "" }
func validTime(t time.Time) bool {
	return t.Year() >= 1 && t.Year() <= 9999 && t.Nanosecond()%int(time.Millisecond) == 0
}
func copyInt(v *int) *int {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}
func (w *Workout) Validate() error {
	if w == nil {
		return trainingErrors.ErrInvalidWorkout
	}
	for _, id := range []uuid.UUID{w.ID, w.AccountID, w.RoutineVersionID, w.RoutineDayID, w.OriginDeviceID} {
		if !protocol.ValidID(id) {
			return trainingErrors.ErrInvalidWorkout
		}
	}
	if w.ID == w.RoutineVersionID || w.ID == w.RoutineDayID || !requiredText(w.RoutineTitleSnapshot, 120) || !requiredText(w.DayLabelSnapshot, 80) || !validText(w.Timezone, 1, 80) || w.Timezone == "Local" || strings.TrimSpace(w.Timezone) != w.Timezone || w.Revision < 1 || !validTime(w.StartedAt) || !validTime(w.CreatedAt) || !validTime(w.UpdatedAt) || w.UpdatedAt.Before(w.CreatedAt) || len(w.Exercises) < 1 || len(w.Exercises) > 50 {
		return trainingErrors.ErrInvalidWorkout
	}
	if (w.State != "in_progress" && w.State != "completed") || (w.State == "completed") != (w.FinishedAt != nil) || w.FinishedAt != nil && (!validTime(*w.FinishedAt) || w.FinishedAt.Before(w.StartedAt)) || w.DeletedAt != nil && !validTime(*w.DeletedAt) || w.FinalizedRevision != nil && (*w.FinalizedRevision < 1 || *w.FinalizedRevision > w.Revision) {
		return trainingErrors.ErrInvalidWorkout
	}
	seen := map[uuid.UUID]bool{w.ID: true, w.RoutineVersionID: true, w.RoutineDayID: true}
	sources := map[uuid.UUID]bool{}
	for i, e := range w.Exercises {
		if !protocol.ValidID(e.ID) || seen[e.ID] || !protocol.ValidID(e.SourceExerciseID) || sources[e.SourceExerciseID] || e.ID == e.SourceExerciseID || e.Position != i || !canonicalID(e.Reference.ID) || (e.Reference.Source != "personal" && e.Reference.Source != "catalog") || !requiredText(e.NameSnapshot, 120) || (e.LoadModeSnapshot != "external" && e.LoadModeSnapshot != "none") || e.PrescribedSets < 1 || e.PrescribedSets > 100 || !requiredText(e.PrescribedReps, 80) || !validText(e.Notes, 0, 1000) || e.RestSeconds != nil && (*e.RestSeconds < 0 || *e.RestSeconds > 3600) {
			return trainingErrors.ErrInvalidWorkout
		}
		seen[e.ID] = true
		sources[e.SourceExerciseID] = true
	}
	for id := range sources {
		if seen[id] {
			return trainingErrors.ErrInvalidWorkout
		}
	}
	return nil
}
func (w *Workout) ValidateCreation(owner uuid.UUID) error {
	if w.Validate() != nil || w.AccountID != owner || w.State != "in_progress" || w.Revision != 1 || w.FinishedAt != nil || w.FinalizedRevision != nil || w.NeedsReview || w.DeletedAt != nil || !w.CreatedAt.Equal(w.UpdatedAt) {
		return trainingErrors.ErrInvalidWorkout
	}
	return nil
}

// IANA membership is validated by the application port; this constructor only
// consumes values and immutable material, with no clock, filesystem or I/O.
func NewStarted(id, owner, origin uuid.UUID, p StartPayload, v *routine.Version, now time.Time) (*Workout, error) {
	if p.validate() != nil || v.Validate() != nil || v.AccountID != owner || v.ID.String() != p.RoutineVersionID {
		return nil, trainingErrors.ErrInvalidWorkout
	}
	expected, _ := hex.DecodeString(p.VersionHash)
	if subtle.ConstantTimeCompare(expected, v.ContentHash[:]) != 1 {
		return nil, trainingErrors.ErrInvalidWorkout
	}
	var day *routine.PublishedDay
	for i := range v.Days {
		if v.Days[i].ID.String() == p.RoutineDayID {
			day = &v.Days[i]
			break
		}
	}
	if day == nil || len(p.Exercises) != len(day.Exercises) {
		return nil, trainingErrors.ErrInvalidWorkout
	}
	mapping := map[uuid.UUID]uuid.UUID{}
	seen := map[uuid.UUID]bool{id: true, v.ID: true, v.RoutineID: true}
	// Every destination is new relative to the complete version, not only the
	// selected day. Database uniqueness separately covers previous workouts.
	if id == v.ID || id == v.RoutineID {
		return nil, trainingErrors.ErrInvalidWorkout
	}
	for _, d := range v.Days {
		if id == d.ID {
			return nil, trainingErrors.ErrInvalidWorkout
		}
		seen[d.ID] = true
		for _, e := range d.Exercises {
			if id == e.ID {
				return nil, trainingErrors.ErrInvalidWorkout
			}
			seen[e.ID] = true
		}
	}
	for _, m := range p.Exercises {
		source, target := uuid.MustParse(m.SourceExerciseID), uuid.MustParse(m.ID)
		if _, ok := mapping[source]; ok || seen[target] {
			return nil, trainingErrors.ErrInvalidWorkout
		}
		mapping[source] = target
		seen[target] = true
	}
	started, _ := parseTimestamp(p.StartedAt)
	w := &Workout{ID: id, AccountID: owner, OriginDeviceID: origin, RoutineVersionID: v.ID, RoutineDayID: day.ID, RoutineTitleSnapshot: v.TitleSnapshot, DayLabelSnapshot: day.Label, State: "in_progress", Timezone: p.Timezone, StartedAt: started, CreatedAt: now.UTC().Truncate(time.Millisecond), UpdatedAt: now.UTC().Truncate(time.Millisecond), Revision: 1, Exercises: make([]Exercise, len(day.Exercises))}
	for i, e := range day.Exercises {
		target, ok := mapping[e.ID]
		if !ok {
			return nil, trainingErrors.ErrInvalidWorkout
		}
		w.Exercises[i] = Exercise{ID: target, SourceExerciseID: e.ID, Reference: e.Reference, NameSnapshot: e.NameSnapshot, LoadModeSnapshot: e.LoadModeSnapshot, Position: i, PrescribedSets: e.PrescribedSets, PrescribedReps: e.PrescribedReps, RestSeconds: copyInt(e.RestSeconds), Notes: e.Notes}
	}
	if w.ValidateCreation(owner) != nil {
		return nil, trainingErrors.ErrInvalidWorkout
	}
	return w, nil
}

// MatchesVersion defends the repository boundary against invented snapshots.
// Availability of current source exercises or the routine root is irrelevant.
func (w *Workout) MatchesVersion(v *routine.Version) bool {
	if w.Validate() != nil || v.Validate() != nil || v.ID != w.RoutineVersionID || v.AccountID != w.AccountID || v.TitleSnapshot != w.RoutineTitleSnapshot {
		return false
	}
	used := map[uuid.UUID]bool{v.ID: true, v.RoutineID: true}
	for _, day := range v.Days {
		used[day.ID] = true
		for _, e := range day.Exercises {
			used[e.ID] = true
		}
	}
	if used[w.ID] {
		return false
	}
	for _, e := range w.Exercises {
		if used[e.ID] {
			return false
		}
	}
	for _, d := range v.Days {
		if d.ID != w.RoutineDayID {
			continue
		}
		if d.Label != w.DayLabelSnapshot || len(d.Exercises) != len(w.Exercises) {
			return false
		}
		for i, e := range d.Exercises {
			x := w.Exercises[i]
			if x.SourceExerciseID != e.ID || x.Reference != e.Reference || x.NameSnapshot != e.NameSnapshot || x.LoadModeSnapshot != e.LoadModeSnapshot || x.Position != e.Position || x.PrescribedSets != e.PrescribedSets || x.PrescribedReps != e.PrescribedReps || x.Notes != e.Notes || (x.RestSeconds == nil) != (e.RestSeconds == nil) || x.RestSeconds != nil && *x.RestSeconds != *e.RestSeconds {
				return false
			}
		}
		return true
	}
	return false
}

type ExerciseDTO struct {
	protocol.PublishedExerciseDTO
	SourceExerciseID string `json:"source_exercise_id"`
}
type DTO struct {
	ID                   string        `json:"id"`
	Revision             string        `json:"revision"`
	CreatedAt            string        `json:"created_at"`
	UpdatedAt            string        `json:"updated_at"`
	DeletedAt            *string       `json:"deleted_at"`
	RoutineVersionID     string        `json:"routine_version_id"`
	RoutineDayID         string        `json:"routine_day_id"`
	RoutineTitleSnapshot string        `json:"routine_title_snapshot"`
	DayLabelSnapshot     string        `json:"day_label_snapshot"`
	State                string        `json:"state"`
	StartedAt            string        `json:"started_at"`
	FinishedAt           *string       `json:"finished_at"`
	Timezone             string        `json:"timezone"`
	FinalizedRevision    *string       `json:"finalized_revision"`
	NeedsReview          bool          `json:"needs_review"`
	OriginDeviceID       string        `json:"origin_device_id"`
	Exercises            []ExerciseDTO `json:"exercises"`
}

func (w *Workout) DTO() DTO {
	out := DTO{ID: w.ID.String(), Revision: strconv.FormatInt(w.Revision, 10), CreatedAt: protocol.Timestamp(w.CreatedAt), UpdatedAt: protocol.Timestamp(w.UpdatedAt), RoutineVersionID: w.RoutineVersionID.String(), RoutineDayID: w.RoutineDayID.String(), RoutineTitleSnapshot: w.RoutineTitleSnapshot, DayLabelSnapshot: w.DayLabelSnapshot, State: w.State, StartedAt: protocol.Timestamp(w.StartedAt), Timezone: w.Timezone, NeedsReview: w.NeedsReview, OriginDeviceID: w.OriginDeviceID.String(), Exercises: make([]ExerciseDTO, len(w.Exercises))}
	if w.FinishedAt != nil {
		s := protocol.Timestamp(*w.FinishedAt)
		out.FinishedAt = &s
	}
	if w.DeletedAt != nil {
		s := protocol.Timestamp(*w.DeletedAt)
		out.DeletedAt = &s
	}
	if w.FinalizedRevision != nil {
		s := strconv.FormatInt(*w.FinalizedRevision, 10)
		out.FinalizedRevision = &s
	}
	for i, e := range w.Exercises {
		out.Exercises[i] = ExerciseDTO{PublishedExerciseDTO: protocol.PublishedExerciseDTO{ID: e.ID.String(), ExerciseRef: protocol.ExerciseRefDTO{Source: e.Reference.Source, ID: e.Reference.ID}, NameSnapshot: e.NameSnapshot, LoadModeSnapshot: e.LoadModeSnapshot, Position: e.Position, PrescribedSets: e.PrescribedSets, PrescribedReps: e.PrescribedReps, RestSeconds: copyInt(e.RestSeconds), Notes: e.Notes}, SourceExerciseID: e.SourceExerciseID.String()}
	}
	return out
}
