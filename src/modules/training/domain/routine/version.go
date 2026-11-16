package routine

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

type PublishedExercise struct {
	ID                             uuid.UUID
	Reference                      ExerciseRef
	NameSnapshot, LoadModeSnapshot string
	Position, PrescribedSets       int
	PrescribedReps                 string
	RestSeconds                    *int
	Notes                          string
}
type PublishedDay struct {
	ID        uuid.UUID
	Label     string
	Position  int
	Exercises []PublishedExercise
}
type Version struct {
	ID, AccountID, RoutineID, ActorDeviceID uuid.UUID
	VersionNumber                           int
	TitleSnapshot                           string
	SourceDraftRevision                     int64
	ContentHash                             [32]byte
	PublishedAt                             time.Time
	Days                                    []PublishedDay
}

func publicationText(value string, max int) bool {
	return validText(value, max) && strings.TrimSpace(value) != ""
}
func (v *Version) Validate() error {
	if v == nil || !protocol.ValidID(v.ID) || !protocol.ValidID(v.AccountID) || !protocol.ValidID(v.RoutineID) || !protocol.ValidID(v.ActorDeviceID) || v.ID == v.RoutineID || v.VersionNumber < 1 || v.VersionNumber > math.MaxInt32 || v.SourceDraftRevision < 1 || !publicationText(v.TitleSnapshot, 120) || v.PublishedAt.IsZero() || v.PublishedAt.Year() < 1 || v.PublishedAt.Year() > 9999 || v.PublishedAt.Nanosecond()%int(time.Millisecond) != 0 || v.ContentHash == [32]byte{} || len(v.Days) < 1 || len(v.Days) > 14 {
		return trainingErrors.ErrInvalidRoutine
	}
	seen := map[uuid.UUID]bool{v.ID: true, v.RoutineID: true}
	claim := func(id uuid.UUID) bool {
		if !protocol.ValidID(id) || seen[id] {
			return false
		}
		seen[id] = true
		return true
	}
	for i, day := range v.Days {
		if !claim(day.ID) || day.Position != i || !publicationText(day.Label, 80) || len(day.Exercises) < 1 || len(day.Exercises) > 50 {
			return trainingErrors.ErrInvalidRoutine
		}
		for j, e := range day.Exercises {
			if !claim(e.ID) || e.Position != j || !validID(e.Reference.ID) || (e.Reference.Source != "personal" && e.Reference.Source != "catalog") || !publicationText(e.NameSnapshot, 120) || (e.LoadModeSnapshot != "external" && e.LoadModeSnapshot != "none") || e.PrescribedSets < 1 || e.PrescribedSets > 100 || !publicationText(e.PrescribedReps, 80) || !validText(e.Notes, 1000) || (e.RestSeconds != nil && (*e.RestSeconds < 0 || *e.RestSeconds > 3600)) {
				return trainingErrors.ErrInvalidRoutine
			}
		}
	}
	return nil
}

// DTO is the complete immutable transport, with an explicit revision of one.
func (v *Version) DTO() protocol.RoutineVersionDTO {
	out := protocol.RoutineVersionDTO{ID: v.ID.String(), RoutineID: v.RoutineID.String(), Revision: "1", VersionNumber: v.VersionNumber, TitleSnapshot: v.TitleSnapshot, FormatVersion: 1, SourceDraftRevision: strconv.FormatInt(v.SourceDraftRevision, 10), ContentHash: hex.EncodeToString(v.ContentHash[:]), PublishedAt: protocol.Timestamp(v.PublishedAt), Days: make([]protocol.PublishedDayDTO, len(v.Days))}
	for i, d := range v.Days {
		out.Days[i] = protocol.PublishedDayDTO{ID: d.ID.String(), Label: d.Label, Position: d.Position, Exercises: make([]protocol.PublishedExerciseDTO, len(d.Exercises))}
		for j, e := range d.Exercises {
			var rest *int
			if e.RestSeconds != nil {
				n := *e.RestSeconds
				rest = &n
			}
			out.Days[i].Exercises[j] = protocol.PublishedExerciseDTO{ID: e.ID.String(), ExerciseRef: protocol.ExerciseRefDTO{Source: e.Reference.Source, ID: e.Reference.ID}, NameSnapshot: e.NameSnapshot, LoadModeSnapshot: e.LoadModeSnapshot, Position: e.Position, PrescribedSets: e.PrescribedSets, PrescribedReps: e.PrescribedReps, RestSeconds: rest, Notes: e.Notes}
		}
	}
	return out
}
func (v *Version) sealHash(canonicalizer protocol.Canonicalizer) error {
	raw, err := json.Marshal(v.DTO())
	if err != nil {
		return trainingErrors.ErrInvalidRoutine
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return trainingErrors.ErrInvalidRoutine
	}
	delete(fields, "content_hash")
	raw, err = json.Marshal(fields)
	if err != nil {
		return trainingErrors.ErrInvalidRoutine
	}
	// This is an internal serialization, not an HTTP request. Canonicalize
	// before the transport helper's raw bound so Go's optional HTML escapes
	// cannot make an otherwise valid canonical version appear oversized.
	if canonicalizer == nil {
		return trainingErrors.ErrInvalidRoutine
	}
	raw, err = canonicalizer.Canonicalize(raw)
	if err != nil {
		return trainingErrors.ErrInvalidRoutine
	}
	dto, err := protocol.BuildRoutineVersion(raw, canonicalizer)
	if err != nil {
		return err
	}
	decoded, err := hex.DecodeString(dto.ContentHash)
	if err != nil || len(decoded) != 32 {
		return trainingErrors.ErrInvalidRoutine
	}
	copy(v.ContentHash[:], decoded)
	return v.Validate()
}
