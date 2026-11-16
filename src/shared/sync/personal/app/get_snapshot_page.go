package app

import (
	"context"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
	"time"
)

type GetSnapshotPageDependencies struct {
	UoW       shared.UnitOfWork
	Snapshots repository.Snapshots
	Now       func() time.Time
}
type GetSnapshotPage struct{ d GetSnapshotPageDependencies }

func NewGetSnapshotPage(d GetSnapshotPageDependencies) (*GetSnapshotPage, error) {
	if d.UoW == nil || d.Snapshots == nil || d.Now == nil {
		return nil, protocol.ErrCommandInvalid
	}
	return &GetSnapshotPage{d: d}, nil
}
func (u *GetSnapshotPage) Execute(ctx context.Context, actor protocol.Actor, id uuid.UUID, number int) (out protocol.SnapshotPage, err error) {
	if !validSnapshotActor(actor) {
		return out, protocol.ErrCommandActor
	}
	if !protocol.ValidID(id) || number < 0 || number >= protocol.SnapshotMaxPages {
		return out, protocol.ErrCommandInvalid
	}
	ctx, err = shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = u.d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if e := u.d.Snapshots.RequireActor(tx, actor, false); e != nil {
			return e
		}
		state, e := u.d.Snapshots.State(tx, actor.AccountID, false)
		if e != nil {
			return e
		}
		s, e := u.d.Snapshots.Get(tx, actor.AccountID, id, false)
		if e != nil {
			return e
		}
		if e = availableSnapshot(s, state, u.d.Now().UTC()); e != nil {
			return e
		}
		if number >= s.Manifest.PageCount {
			return protocol.ErrSnapshotMissing
		}
		out, e = u.d.Snapshots.Page(tx, actor.AccountID, id, number)
		if e != nil {
			return e
		}
		if out.Hash != s.Manifest.PageHashes[number] {
			return protocol.ErrCommandPersistence
		}
		return nil
	})
	if err != nil {
		return protocol.SnapshotPage{}, err
	}
	return out, nil
}
