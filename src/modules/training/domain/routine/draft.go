package routine

import (
	"strings"
	"unicode/utf8"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

type ExerciseRef struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}
type DraftExercise struct {
	ID             string       `json:"id"`
	ExerciseRef    *ExerciseRef `json:"exercise_ref"`
	PrescribedSets *int         `json:"prescribed_sets"`
	PrescribedReps string       `json:"prescribed_reps"`
	RestSeconds    *int         `json:"rest_seconds"`
	Notes          string       `json:"notes"`
}
type DraftDay struct {
	ID        string          `json:"id"`
	Label     string          `json:"label"`
	Exercises []DraftExercise `json:"exercises"`
}
type Draft struct {
	FormatVersion int        `json:"format_version"`
	Title         string     `json:"title"`
	Days          []DraftDay `json:"days"`
}

func validID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && protocol.ValidID(id) && id.String() == value
}
func validText(value string, max int) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0) && utf8.RuneCountInString(value) <= max
}

// Validate accepts deliberately incomplete, typed editorial content. It does
// not apply publication rules or invent labels, prescribed sets or references.
func (d Draft) Validate() error {
	if d.FormatVersion != 1 || !validText(d.Title, 120) || d.Days == nil || len(d.Days) > 14 {
		return trainingErrors.ErrInvalidRoutine
	}
	ids := map[string]bool{}
	claim := func(id string) bool {
		if !validID(id) || ids[id] {
			return false
		}
		ids[id] = true
		return true
	}
	for _, day := range d.Days {
		if !claim(day.ID) || !validText(day.Label, 80) || day.Exercises == nil || len(day.Exercises) > 50 {
			return trainingErrors.ErrInvalidRoutine
		}
		for _, e := range day.Exercises {
			if !claim(e.ID) || !validText(e.PrescribedReps, 80) || !validText(e.Notes, 1000) || (e.PrescribedSets != nil && (*e.PrescribedSets < 1 || *e.PrescribedSets > 100)) || (e.RestSeconds != nil && (*e.RestSeconds < 0 || *e.RestSeconds > 3600)) {
				return trainingErrors.ErrInvalidRoutine
			}
			if r := e.ExerciseRef; r != nil && (!validID(r.ID) || (r.Source != "personal" && r.Source != "catalog")) {
				return trainingErrors.ErrInvalidRoutine
			}
		}
	}
	return nil
}

// Clone prevents mutable UI/command slices from changing a constructed aggregate.
func (d Draft) Clone() Draft {
	out := Draft{FormatVersion: d.FormatVersion, Title: d.Title, Days: make([]DraftDay, len(d.Days))}
	for i, day := range d.Days {
		out.Days[i] = DraftDay{ID: day.ID, Label: day.Label, Exercises: make([]DraftExercise, len(day.Exercises))}
		for j, e := range day.Exercises {
			copy := e
			if e.ExerciseRef != nil {
				ref := *e.ExerciseRef
				copy.ExerciseRef = &ref
			}
			if e.PrescribedSets != nil {
				n := *e.PrescribedSets
				copy.PrescribedSets = &n
			}
			if e.RestSeconds != nil {
				n := *e.RestSeconds
				copy.RestSeconds = &n
			}
			out.Days[i].Exercises[j] = copy
		}
	}
	return out
}
func (d Draft) References() []ExerciseRef {
	refs := []ExerciseRef{}
	for _, day := range d.Days {
		for _, e := range day.Exercises {
			if e.ExerciseRef != nil {
				refs = append(refs, *e.ExerciseRef)
			}
		}
	}
	return refs
}
