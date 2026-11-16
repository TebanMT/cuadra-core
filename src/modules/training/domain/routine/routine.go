package routine

import (
	"math"
	"time"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

type Routine struct {
	ID, AccountID                             uuid.UUID
	Title, Status                             string
	Revision                                  int64
	CurrentVersionID, DuplicatedFromVersionID *uuid.UUID
	CreatedAt, UpdatedAt                      time.Time
	DeletedAt                                 *time.Time
}
type DraftRecord struct {
	RoutineID, AccountID uuid.UUID
	Revision             int64
	BaseVersionID        *uuid.UUID
	Content              *Draft
	CreatedAt, UpdatedAt time.Time
	DeletedAt            *time.Time
}

func (r *Routine) Validate() error {
	if r == nil || !protocol.ValidID(r.ID) || !protocol.ValidID(r.AccountID) || !validText(r.Title, 120) || r.Revision < 1 || !validTimes(r.CreatedAt, r.UpdatedAt, r.DeletedAt) {
		return trainingErrors.ErrInvalidRoutine
	}
	if r.Status != "draft" && r.Status != "ready" && r.Status != "archived" {
		return trainingErrors.ErrInvalidRoutine
	}
	if r.Status == "ready" && r.CurrentVersionID == nil {
		return trainingErrors.ErrInvalidRoutine
	}
	for _, id := range []*uuid.UUID{r.CurrentVersionID, r.DuplicatedFromVersionID} {
		if id != nil && !protocol.ValidID(*id) {
			return trainingErrors.ErrInvalidRoutine
		}
	}
	return nil
}
func (d *DraftRecord) Validate() error {
	if d == nil || !protocol.ValidID(d.RoutineID) || !protocol.ValidID(d.AccountID) || d.Revision < 1 || !validTimes(d.CreatedAt, d.UpdatedAt, d.DeletedAt) || (d.Content == nil) != (d.DeletedAt != nil) || (d.BaseVersionID != nil && !protocol.ValidID(*d.BaseVersionID)) {
		return trainingErrors.ErrInvalidRoutine
	}
	if d.Content != nil {
		return d.Content.Validate()
	}
	return nil
}
func validTimes(created, updated time.Time, deleted *time.Time) bool {
	return !created.IsZero() && created.Year() >= 1 && created.Year() <= 9999 && !updated.Before(created) && updated.Year() <= 9999 && (deleted == nil || (!deleted.Before(created) && deleted.Year() <= 9999))
}

// Save changes only the editable draft. Reopening a discarded draft retains
// its identity and advances its revision; neither case publishes a routine.
func (d *DraftRecord) Save(content Draft, baseVersionID *uuid.UUID, expectedRevision int64, now time.Time) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if expectedRevision < 1 || expectedRevision == math.MaxInt64 || d.Revision != expectedRevision {
		return trainingErrors.ErrRoutineRevisionConflict
	}
	if err := content.Validate(); err != nil {
		return err
	}
	if baseVersionID != nil && !protocol.ValidID(*baseVersionID) {
		return trainingErrors.ErrInvalidRoutine
	}
	now = now.UTC().Truncate(time.Millisecond)
	if now.IsZero() || now.Before(d.UpdatedAt) || now.Year() > 9999 {
		return trainingErrors.ErrInvalidRoutine
	}
	copy := content.Clone()
	var base *uuid.UUID
	if baseVersionID != nil {
		value := *baseVersionID
		base = &value
	}
	d.Content = &copy
	d.BaseVersionID = base
	d.Revision++
	d.UpdatedAt = now
	d.DeletedAt = nil
	return nil
}

func (d *DraftRecord) ValidateSave(ownerID uuid.UUID, expectedRevision int64) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if d.AccountID != ownerID || !protocol.ValidID(ownerID) || expectedRevision < 1 || expectedRevision == math.MaxInt64 || d.Revision != expectedRevision+1 || d.DeletedAt != nil || d.Content == nil || d.UpdatedAt.Nanosecond()%int(time.Millisecond) != 0 {
		return trainingErrors.ErrInvalidRoutine
	}
	return nil
}

func NewDraftRoutine(id, ownerID uuid.UUID, content Draft, now time.Time) (*Routine, *DraftRecord, error) {
	if err := content.Validate(); err != nil {
		return nil, nil, err
	}
	now = now.UTC().Truncate(time.Millisecond)
	copy := content.Clone()
	r := &Routine{ID: id, AccountID: ownerID, Title: copy.Title, Status: "draft", Revision: 1, CreatedAt: now, UpdatedAt: now}
	d := &DraftRecord{RoutineID: id, AccountID: ownerID, Revision: 1, Content: &copy, CreatedAt: now, UpdatedAt: now}
	if err := ValidateCreation(ownerID, r, d); err != nil {
		return nil, nil, err
	}
	return r, d, nil
}

func ValidateCreation(ownerID uuid.UUID, r *Routine, d *DraftRecord) error {
	if r == nil || d == nil || !protocol.ValidID(ownerID) || r.AccountID != ownerID || d.AccountID != ownerID || !protocol.ValidID(r.ID) || r.ID != d.RoutineID || r.Revision != 1 || d.Revision != 1 || r.Status != "draft" || r.CurrentVersionID != nil || r.DuplicatedFromVersionID != nil || r.DeletedAt != nil || d.DeletedAt != nil || d.BaseVersionID != nil || d.Content == nil {
		return trainingErrors.ErrInvalidRoutine
	}
	if r.CreatedAt.IsZero() || r.CreatedAt.Year() < 1 || r.CreatedAt.Year() > 9999 || r.CreatedAt.Nanosecond()%int(time.Millisecond) != 0 || !r.CreatedAt.Equal(r.UpdatedAt) || !r.CreatedAt.Equal(d.CreatedAt) || !r.CreatedAt.Equal(d.UpdatedAt) || r.Title != d.Content.Title {
		return trainingErrors.ErrInvalidRoutine
	}
	return d.Content.Validate()
}
