package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
)

const routinesCursorOrder = "created_at.desc-id.desc.v1"

type ListRoutinesDependencies struct {
	UoW      shared.UnitOfWork
	Commands protocolRepository.Commands
	Stream   protocolRepository.Stream
	Routines repository.RoutineListRepository
	Cursors  protocol.CursorSigner
	Now      func() time.Time
}
type ListRoutinesInput struct {
	Cursor, Status, Query string
	Limit                 int
}
type RoutineListResult struct {
	Items      []RoutineDTO `json:"items"`
	NextCursor *string      `json:"next_cursor"`
}
type ListRoutines struct{ d ListRoutinesDependencies }

func NewListRoutines(d ListRoutinesDependencies) (*ListRoutines, error) {
	if d.UoW == nil || d.Commands == nil || d.Stream == nil || d.Routines == nil || d.Cursors == nil || d.Now == nil {
		return nil, trainingErrors.ErrTrainingConfiguration
	}
	return &ListRoutines{d}, nil
}
func (u *ListRoutines) Execute(ctx context.Context, actor security.AccessClaims, in ListRoutinesInput) (RoutineListResult, error) {
	if in.Limit == 0 {
		in.Limit = 50
	}
	if in.Limit < 1 || in.Limit > 100 || len(in.Cursor) > protocol.CursorMaximumLength {
		return RoutineListResult{}, trainingErrors.ErrInvalidRoutine
	}
	filter, err := routine.NormalizeListFilter(in.Status, in.Query)
	if err != nil {
		return RoutineListResult{}, err
	}
	out := RoutineListResult{Items: []RoutineDTO{}}
	err = routineRead(ctx, u.d.UoW, u.d.Commands, actor, func(tx shared.Transaction) error {
		state, err := u.d.Stream.State(tx, actor.AccountID)
		if err != nil {
			return err
		}
		now := u.d.Now().UTC()
		// A digest fits the existing cursor bound even for 120 Unicode scalars
		// that expand substantially under canonical decomposition.
		hash := sha256.Sum256([]byte(filter.Query))
		binding := protocol.CursorBinding{AccountID: actor.AccountID, Epoch: state.Epoch, Query: protocol.CursorRoutines, Filters: map[string]string{"status": filter.Status, "query_hash": hex.EncodeToString(hash[:])}, Order: routinesCursorOrder}
		if binding.Validate() != nil || now.IsZero() || now.Year() < 1 || now.Year() > 9999 {
			return protocol.ErrCursorInvalid
		}
		var after *routine.ListPosition
		if in.Cursor != "" {
			position, err := u.d.Cursors.Verify(in.Cursor, binding, now)
			if err != nil {
				return err
			}
			at, err := time.Parse(time.RFC3339Nano, position.Key)
			if err != nil || at.UTC().Format(time.RFC3339Nano) != position.Key || position.Sequence != "" {
				return protocol.ErrCursorInvalid
			}
			after = &routine.ListPosition{CreatedAt: at, ID: position.ID}
			if after.Validate() != nil {
				return protocol.ErrCursorInvalid
			}
		}
		items, err := u.d.Routines.List(tx, actor.AccountID, filter, after, in.Limit+1)
		if err != nil {
			return err
		}
		if len(items) > in.Limit+1 {
			return trainingErrors.ErrTrainingPersistence
		}
		previous := after
		for _, item := range items {
			if item.Validate() != nil || item.AccountID != actor.AccountID || item.DeletedAt != nil || filter.Status == "active" && item.Status == "archived" || filter.Status == "archived" && item.Status != "archived" || !strings.Contains(routine.NormalizeSearch(item.Title), filter.Query) {
				return trainingErrors.ErrTrainingPersistence
			}
			if previous != nil && (item.CreatedAt.After(previous.CreatedAt) || item.CreatedAt.Equal(previous.CreatedAt) && item.ID.String() >= previous.ID.String()) {
				return trainingErrors.ErrTrainingPersistence
			}
			previous = &routine.ListPosition{CreatedAt: item.CreatedAt, ID: item.ID}
		}
		if len(items) > in.Limit {
			items = items[:in.Limit]
			last := items[len(items)-1]
			next, err := u.d.Cursors.Issue(binding, protocol.CursorPosition{ID: last.ID, Key: last.CreatedAt.UTC().Format(time.RFC3339Nano)}, now)
			if err != nil {
				return err
			}
			out.NextCursor = &next
		}
		for _, item := range items {
			out.Items = append(out.Items, routineView(item))
		}
		return nil
	})
	if err != nil {
		return RoutineListResult{}, err
	}
	return out, nil
}
