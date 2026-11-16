package app

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"time"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

type PullDependencies struct {
	UoW           shared.UnitOfWork
	Commands      repository.Commands
	Stream        repository.Stream
	Cursors       protocol.CursorSigner
	Canonicalizer protocol.Canonicalizer
	Now           func() time.Time
}
type PullInput struct {
	Cursor string
	Limit  int
}
type Pull struct{ d PullDependencies }

func NewPull(d PullDependencies) (*Pull, error) {
	if d.UoW == nil || d.Commands == nil || d.Stream == nil || d.Cursors == nil || d.Canonicalizer == nil || d.Now == nil {
		return nil, protocol.ErrCommandInvalid
	}
	return &Pull{d: d}, nil
}
func (u *Pull) Execute(ctx context.Context, actor protocol.Actor, in PullInput) (protocol.PullResult, error) {
	if !protocol.ValidID(actor.AccountID) || !protocol.ValidID(actor.SessionID) || !protocol.ValidID(actor.DeviceID) {
		return protocol.PullResult{}, protocol.ErrCommandActor
	}
	if in.Limit == 0 {
		in.Limit = 50
	}
	if in.Limit < 1 || in.Limit > 100 || len(in.Cursor) > protocol.CursorMaximumLength {
		return protocol.PullResult{}, protocol.ErrCommandInvalid
	}
	ctx, err := shared.WithAuthenticatedPersonalAccount(ctx, actor.AccountID)
	if err != nil {
		return protocol.PullResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out protocol.PullResult
	err = u.d.UoW.Command(ctx, func(tx shared.Transaction) error {
		// Serializing this bounded read with account mutations means revocation
		// and compaction cannot race the actor check or advance the page cutoff.
		if err := u.d.Commands.RequireActor(tx, actor, actor.DeviceID); err != nil {
			return err
		}
		state, err := u.d.Stream.State(tx, actor.AccountID)
		if err != nil {
			return err
		}
		if in.Cursor == "" {
			return protocol.ErrCursorExpired
		}
		now := u.d.Now().UTC()
		binding := protocol.PullBinding(actor.AccountID, state.Epoch)
		position, err := u.d.Cursors.Verify(in.Cursor, binding, now)
		if err != nil {
			return err
		}
		after, err := protocol.ParseRevision(position.Sequence)
		if err != nil || after > state.LastSequence {
			return protocol.ErrCursorInvalid
		}
		if after < state.CompactedThrough {
			return protocol.ErrCursorExpired
		}
		groups, err := u.d.Stream.Groups(tx, actor.AccountID, after, in.Limit+1)
		if err != nil {
			return err
		}
		selected, last, err := selectGroups(groups, after, state.LastSequence, in.Limit, u.d.Canonicalizer)
		if err != nil {
			return err
		}
		cursor, err := u.d.Cursors.Issue(binding, protocol.CursorPosition{Sequence: strconv.FormatInt(last, 10)}, now)
		if err != nil {
			return err
		}
		out = protocol.PullResult{ProtocolVersion: 1, Epoch: state.Epoch.String(), Groups: selected, NextCursor: cursor, HasMore: last < state.LastSequence}
		return nil
	})
	if err != nil {
		return protocol.PullResult{}, err
	}
	return out, nil
}

func selectGroups(groups []protocol.ChangeGroup, after, high int64, limit int, c protocol.Canonicalizer) ([]protocol.ChangeGroup, int64, error) {
	out := make([]protocol.ChangeGroup, 0)
	last := after
	bytes := 2
	if len(groups) == 0 && after != high {
		return nil, after, protocol.ErrCommandPersistence
	}
	for _, g := range groups {
		seq, err := protocol.ParseRevision(g.Sequence)
		op, opErr := uuid.Parse(g.OperationID)
		if err != nil || last == math.MaxInt64 || seq != last+1 || seq > high || opErr != nil || !protocol.ValidID(op) || op.String() != g.OperationID || len(g.Changes) < 1 || len(g.Changes) > 1004 {
			return nil, after, protocol.ErrCommandPersistence
		}
		for _, change := range g.Changes {
			id, idErr := uuid.Parse(change.EntityID)
			revision, revErr := protocol.ParseRevision(change.Revision)
			if idErr != nil || !protocol.ValidID(id) || id.String() != change.EntityID || revErr != nil || revision < 1 || !json.Valid(change.Data) {
				return nil, after, protocol.ErrCommandPersistence
			}
			switch change.EntityType {
			case "profile", "personal_exercise", "routine", "routine_draft", "routine_version", "workout", "workout_set", "conflict":
			default:
				return nil, after, protocol.ErrCommandPersistence
			}
		}
		raw, err := json.Marshal(g)
		if err != nil {
			return nil, after, protocol.ErrCommandPersistence
		}
		canonical, err := c.Canonicalize(raw)
		if err != nil || len(canonical) > protocol.PullPageBytes {
			return nil, after, protocol.ErrCommandPersistence
		}
		// The first group stays indivisible even when the surrounding page
		// metadata brings the response slightly above the soft page target.
		if len(out) >= limit || (len(out) > 0 && bytes+1+len(canonical) > protocol.PullPageBytes) {
			break
		}
		if len(out) > 0 {
			bytes++
		}
		bytes += len(canonical)
		out = append(out, g)
		last = seq
	}
	return out, last, nil
}
