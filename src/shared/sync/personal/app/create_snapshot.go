package app

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/me"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

type CreateSnapshotDependencies struct {
	UoW          shared.PersonalSnapshotUnitOfWork
	Snapshots    repository.Snapshots
	Cursors      protocol.CursorSigner
	Capabilities protocol.Capabilities
	Now          func() time.Time
}
type CreateSnapshot struct{ d CreateSnapshotDependencies }

func NewCreateSnapshot(d CreateSnapshotDependencies) (*CreateSnapshot, error) {
	c := d.Capabilities
	configured := me.Capabilities{ProtocolVersion: c.ProtocolVersion, MinimumProtocolVersion: c.MinimumProtocolVersion, MinimumAppVersion: c.MinimumAppVersion, MaxPushOperations: c.MaxPushOperations, MaxRequestBytes: c.MaxRequestBytes, MaxGroupBytes: c.MaxGroupBytes, Features: c.Features, PolicyVersion: c.PolicyVersion}
	if d.UoW == nil || d.Snapshots == nil || d.Cursors == nil || d.Now == nil || configured.Validate() != nil {
		return nil, protocol.ErrCommandInvalid
	}
	d.Capabilities.Features = append([]string{}, c.Features...)
	if c.PolicyVersion != nil {
		v := *c.PolicyVersion
		d.Capabilities.PolicyVersion = &v
	}
	return &CreateSnapshot{d: d}, nil
}

// The request body is fixed at protocol_version=1. Its Idempotency-Key is the
// snapshot ID, so retries retrieve the original manifest without extending it.
func (u *CreateSnapshot) Execute(ctx context.Context, actor protocol.Actor, id uuid.UUID) (out protocol.Snapshot, err error) {
	if !validSnapshotActor(actor) {
		return out, protocol.ErrCommandActor
	}
	if !protocol.ValidID(id) {
		return out, protocol.ErrCommandInvalid
	}
	ctx, err = shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = u.d.UoW.SnapshotCommand(ctx, func(tx shared.Transaction) error {
		if e := u.d.Snapshots.RequireActor(tx, actor, true); e != nil {
			return e
		}
		state, e := u.d.Snapshots.State(tx, actor.AccountID, true)
		if e != nil {
			return e
		}
		now := u.d.Now().UTC().Truncate(time.Millisecond)
		if now.IsZero() || now.Year() < 1 || now.Year() > 9998 {
			return protocol.ErrCommandInvalid
		}
		stored, e := u.d.Snapshots.Get(tx, actor.AccountID, id, true)
		if e == nil {
			if e = availableSnapshot(stored, state, now); e != nil {
				return e
			}
			out = stored.Manifest
			return nil
		}
		if !errors.Is(e, protocol.ErrSnapshotMissing) {
			return e
		}
		hashes, e := u.d.Snapshots.Materialize(tx, actor.AccountID, id)
		if e != nil {
			return e
		}
		cursor, e := u.d.Cursors.Issue(protocol.PullBinding(actor.AccountID, state.Epoch), protocol.CursorPosition{Sequence: strconv.FormatInt(state.LastSequence, 10)}, now)
		if e != nil {
			return e
		}
		caps := u.d.Capabilities
		caps.Features = append([]string{}, caps.Features...)
		if caps.PolicyVersion != nil {
			value := *caps.PolicyVersion
			caps.PolicyVersion = &value
		}
		out = protocol.Snapshot{ID: id.String(), Epoch: state.Epoch.String(), ThroughSequence: strconv.FormatInt(state.LastSequence, 10), PageCount: len(hashes), PageHashes: hashes, ExpiresAt: protocol.Timestamp(now.Add(protocol.SnapshotTTL)), Cursor: cursor, Capabilities: caps}
		return u.d.Snapshots.Save(tx, protocol.StoredSnapshot{ID: id, AccountID: actor.AccountID, Epoch: state.Epoch, ThroughSequence: state.LastSequence, CreatedAt: now, ExpiresAt: now.Add(protocol.SnapshotTTL), Manifest: out})
	})
	if err != nil {
		return protocol.Snapshot{}, err
	}
	return out, nil
}
func validSnapshotActor(a protocol.Actor) bool {
	return protocol.ValidID(a.AccountID) && protocol.ValidID(a.SessionID) && protocol.ValidID(a.DeviceID)
}
func availableSnapshot(s protocol.StoredSnapshot, state protocol.StreamState, now time.Time) error {
	if !now.Before(s.ExpiresAt) {
		return protocol.ErrSnapshotExpired
	}
	if s.Epoch != state.Epoch {
		return protocol.ErrCursorEpochChanged
	}
	if s.ThroughSequence < state.CompactedThrough {
		return protocol.ErrSnapshotExpired
	}
	if s.ThroughSequence > state.LastSequence {
		return protocol.ErrCommandPersistence
	}
	return nil
}
