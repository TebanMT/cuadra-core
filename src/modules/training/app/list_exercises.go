package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/search"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
)

const exercisesCursorOrder = "name.c.asc-source.asc-id.asc.v1"

type ListExercisesDependencies struct {
	UoW       shared.UnitOfWork
	Commands  protocolRepository.Commands
	Stream    protocolRepository.Stream
	Exercises repository.ExerciseListRepository
	Cursors   protocol.CursorSigner
	Now       func() time.Time
}
type ListExercisesInput struct {
	Cursor, Query string
	Limit         int
}
type ExerciseDTO struct {
	ID         string  `json:"id"`
	Revision   string  `json:"revision"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
	DeletedAt  *string `json:"deleted_at"`
	Source     string  `json:"source"`
	Name       string  `json:"name"`
	LoadMode   string  `json:"load_mode"`
	Notes      string  `json:"notes"`
	ArchivedAt *string `json:"archived_at"`
}
type ExerciseListResult struct {
	Items      []ExerciseDTO `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}
type ListExercises struct{ d ListExercisesDependencies }

func NewListExercises(d ListExercisesDependencies) (*ListExercises, error) {
	if d.UoW == nil || d.Commands == nil || d.Stream == nil || d.Exercises == nil || d.Cursors == nil || d.Now == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &ListExercises{d}, nil
}

func (u *ListExercises) Execute(ctx context.Context, actor security.AccessClaims, in ListExercisesInput) (ExerciseListResult, error) {
	if in.Limit == 0 {
		in.Limit = 50
	}
	if in.Limit < 1 || in.Limit > 100 || len(in.Cursor) > protocol.CursorMaximumLength {
		return ExerciseListResult{}, trainingErrors.ErrInvalidExercise
	}
	filter, err := exercise.NormalizeListFilter(in.Query)
	if err != nil {
		return ExerciseListResult{}, err
	}
	out := ExerciseListResult{Items: []ExerciseDTO{}}
	// The existing bounded account-read guard verifies session/device revocation
	// even when the resulting page contains only global catalog exercises.
	err = routineRead(ctx, u.d.UoW, u.d.Commands, actor, func(tx shared.Transaction) error {
		state, err := u.d.Stream.State(tx, actor.AccountID)
		if err != nil {
			return err
		}
		now := u.d.Now().UTC()
		hash := sha256.Sum256([]byte(filter.Query))
		binding := protocol.CursorBinding{AccountID: actor.AccountID, Epoch: state.Epoch, Query: protocol.CursorExercises, Filters: map[string]string{"query_hash": hex.EncodeToString(hash[:])}, Order: exercisesCursorOrder}
		if binding.Validate() != nil || now.IsZero() || now.Year() < 1 || now.Year() > 9999 {
			return protocol.ErrCursorInvalid
		}
		var after *exercise.ListPosition
		if in.Cursor != "" {
			position, err := u.d.Cursors.Verify(in.Cursor, binding, now)
			if err != nil {
				return err
			}
			after, err = exercise.PositionFromCursor(position)
			if err != nil {
				return err
			}
		}
		items, err := u.d.Exercises.List(tx, actor.AccountID, filter, after, in.Limit+1)
		if err != nil {
			return err
		}
		if len(items) > in.Limit+1 {
			return trainingErrors.ErrTrainingPersistence
		}
		previous := after
		for _, item := range items {
			if item.Validate() != nil || item.Source == exercise.PersonalSource && item.AccountID != actor.AccountID || item.ArchivedAt != nil || item.DeletedAt != nil || !strings.Contains(search.Normalize(item.Name), filter.Query) {
				return trainingErrors.ErrTrainingPersistence
			}
			position := item.Position()
			if previous != nil && !position.After(*previous) {
				return trainingErrors.ErrTrainingPersistence
			}
			previous = &position
		}
		if len(items) > in.Limit {
			items = items[:in.Limit]
			last := items[len(items)-1]
			next, err := u.d.Cursors.Issue(binding, last.Position().CursorPosition(), now)
			if err != nil {
				return err
			}
			out.NextCursor = &next
		}
		for _, item := range items {
			out.Items = append(out.Items, exerciseView(item))
		}
		return nil
	})
	if err != nil {
		return ExerciseListResult{}, err
	}
	return out, nil
}

func exerciseView(e *exercise.ListedExercise) ExerciseDTO {
	return ExerciseDTO{ID: e.ID.String(), Revision: strconv.FormatInt(e.Revision, 10), CreatedAt: protocol.Timestamp(e.CreatedAt), UpdatedAt: protocol.Timestamp(e.UpdatedAt), DeletedAt: routineOptionalTime(e.DeletedAt), Source: e.Source, Name: e.Name, LoadMode: string(e.LoadMode), Notes: e.Notes, ArchivedAt: routineOptionalTime(e.ArchivedAt)}
}
