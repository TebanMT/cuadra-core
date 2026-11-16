package routine

import (
	"math"
	"time"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

type ExerciseSnapshot struct {
	Reference      ExerciseRef
	Name, LoadMode string
}
type PublicationMaterial struct {
	NextVersionNumber int
	Exercises         map[string]ExerciseSnapshot
}
type Publication struct {
	ExpectedRootRevision, ExpectedDraftRevision int64
	Root                                        Routine
	Draft                                       DraftRecord
	Version                                     Version
}

func NewPublication(root *Routine, draft *DraftRecord, payload PublishPayload, material PublicationMaterial, actorID uuid.UUID, now time.Time, canonicalizer protocol.Canonicalizer) (*Publication, error) {
	if root.Validate() != nil || draft.Validate() != nil || payload.ValidateShape() != nil || root.AccountID != draft.AccountID || root.ID != draft.RoutineID || root.DeletedAt != nil || root.Status == "archived" || draft.DeletedAt != nil || draft.Content == nil || !protocol.ValidID(actorID) {
		return nil, trainingErrors.ErrInvalidRoutine
	}
	revision, _ := protocol.ParseRevision(payload.DraftRevision)
	if revision != draft.Revision {
		return nil, trainingErrors.ErrRoutineRevisionConflict
	}
	if root.Revision == math.MaxInt64 || draft.Revision == math.MaxInt64 || material.NextVersionNumber < 1 || material.NextVersionNumber > math.MaxInt32 {
		return nil, trainingErrors.ErrTrainingLimit
	}
	now = now.UTC().Truncate(time.Millisecond)
	if now.IsZero() || now.Before(root.UpdatedAt) || now.Before(draft.UpdatedAt) || now.Year() > 9999 {
		return nil, trainingErrors.ErrInvalidRoutine
	}
	content := draft.Content
	if !publicationText(content.Title, 120) || len(content.Days) < 1 || len(payload.Days) != len(content.Days) {
		return nil, trainingErrors.ErrInvalidRoutine
	}
	editingIDs := map[string]bool{root.ID.String(): true}
	count := 0
	for _, day := range content.Days {
		editingIDs[day.ID] = true
		if !publicationText(day.Label, 80) || len(day.Exercises) == 0 {
			return nil, trainingErrors.ErrInvalidRoutine
		}
		for _, e := range day.Exercises {
			editingIDs[e.ID] = true
			count++
			if e.ExerciseRef == nil || e.PrescribedSets == nil || !publicationText(e.PrescribedReps, 80) {
				return nil, trainingErrors.ErrInvalidRoutine
			}
		}
	}
	if len(material.Exercises) != count {
		return nil, trainingErrors.ErrRoutineReference
	}
	newIDs := map[string]bool{}
	claim := func(id string) bool {
		if !validID(id) || newIDs[id] || editingIDs[id] {
			return false
		}
		newIDs[id] = true
		return true
	}
	if !claim(payload.VersionID) {
		return nil, trainingErrors.ErrInvalidRoutine
	}
	maps := map[string]DayIDMap{}
	for _, m := range payload.Days {
		if _, exists := maps[m.DraftDayID]; exists || !claim(m.ID) {
			return nil, trainingErrors.ErrInvalidRoutine
		}
		for _, e := range m.Exercises {
			if !claim(e.ID) {
				return nil, trainingErrors.ErrInvalidRoutine
			}
		}
		maps[m.DraftDayID] = m
	}
	v := Version{ID: uuid.MustParse(payload.VersionID), AccountID: root.AccountID, RoutineID: root.ID, ActorDeviceID: actorID, VersionNumber: material.NextVersionNumber, TitleSnapshot: content.Title, SourceDraftRevision: draft.Revision, PublishedAt: now, Days: make([]PublishedDay, len(content.Days))}
	for i, day := range content.Days {
		m, exists := maps[day.ID]
		if !exists || len(m.Exercises) != len(day.Exercises) {
			return nil, trainingErrors.ErrInvalidRoutine
		}
		byID := map[string]string{}
		for _, e := range m.Exercises {
			if _, exists := byID[e.DraftExerciseID]; exists {
				return nil, trainingErrors.ErrInvalidRoutine
			}
			byID[e.DraftExerciseID] = e.ID
		}
		v.Days[i] = PublishedDay{ID: uuid.MustParse(m.ID), Label: day.Label, Position: i, Exercises: make([]PublishedExercise, len(day.Exercises))}
		for j, e := range day.Exercises {
			id, exists := byID[e.ID]
			if !exists {
				return nil, trainingErrors.ErrInvalidRoutine
			}
			snapshot, exists := material.Exercises[e.ID]
			if !exists || snapshot.Reference != *e.ExerciseRef {
				return nil, trainingErrors.ErrRoutineReference
			}
			var rest *int
			if e.RestSeconds != nil {
				n := *e.RestSeconds
				rest = &n
			}
			v.Days[i].Exercises[j] = PublishedExercise{ID: uuid.MustParse(id), Reference: *e.ExerciseRef, NameSnapshot: snapshot.Name, LoadModeSnapshot: snapshot.LoadMode, Position: j, PrescribedSets: *e.PrescribedSets, PrescribedReps: e.PrescribedReps, RestSeconds: rest, Notes: e.Notes}
		}
	}
	if err := v.sealHash(canonicalizer); err != nil {
		return nil, err
	}
	newRoot := *root
	newRoot.Title = v.TitleSnapshot
	newRoot.Status = "ready"
	newRoot.Revision++
	newRoot.UpdatedAt = now
	versionID := v.ID
	newRoot.CurrentVersionID = &versionID
	if root.DuplicatedFromVersionID != nil {
		id := *root.DuplicatedFromVersionID
		newRoot.DuplicatedFromVersionID = &id
	}
	newDraft := *draft
	newDraft.Revision++
	newDraft.UpdatedAt = now
	newDraft.Content = nil
	deleted := now
	newDraft.DeletedAt = &deleted
	base := v.ID
	newDraft.BaseVersionID = &base
	p := &Publication{ExpectedRootRevision: root.Revision, ExpectedDraftRevision: draft.Revision, Root: newRoot, Draft: newDraft, Version: v}
	return p, p.Validate(root.AccountID)
}

func (p *Publication) Validate(ownerID uuid.UUID) error {
	if p == nil || p.Root.Validate() != nil || p.Draft.Validate() != nil || p.Version.Validate() != nil || p.ExpectedRootRevision < 1 || p.ExpectedRootRevision == math.MaxInt64 || p.ExpectedDraftRevision < 1 || p.ExpectedDraftRevision == math.MaxInt64 {
		return trainingErrors.ErrInvalidRoutine
	}
	r, d, v := &p.Root, &p.Draft, &p.Version
	if !protocol.ValidID(ownerID) || r.AccountID != ownerID || d.AccountID != ownerID || v.AccountID != ownerID || r.ID != d.RoutineID || r.ID != v.RoutineID || r.Revision != p.ExpectedRootRevision+1 || d.Revision != p.ExpectedDraftRevision+1 || v.SourceDraftRevision != p.ExpectedDraftRevision || r.DeletedAt != nil || r.Status != "ready" || r.Title != v.TitleSnapshot || r.CurrentVersionID == nil || *r.CurrentVersionID != v.ID || d.Content != nil || d.DeletedAt == nil || d.BaseVersionID == nil || *d.BaseVersionID != v.ID || !r.UpdatedAt.Equal(v.PublishedAt) || !d.UpdatedAt.Equal(v.PublishedAt) || !d.DeletedAt.Equal(v.PublishedAt) {
		return trainingErrors.ErrInvalidRoutine
	}
	return nil
}
