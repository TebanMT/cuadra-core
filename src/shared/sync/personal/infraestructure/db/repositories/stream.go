//go:build server

package repositories

import (
	"encoding/json"
	"strconv"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

type Stream struct{}

func NewStream() *Stream { return &Stream{} }

var _ repository.Stream = (*Stream)(nil)

func (*Stream) State(handle shared.Transaction, accountID uuid.UUID) (out protocol.StreamState, err error) {
	defer func() {
		err = persistenceError(err)
		if err != nil {
			out = protocol.StreamState{}
		}
	}()
	tx, err := dataConnection(handle, accountID, true)
	if err != nil {
		return out, err
	}
	err = tx.Raw("SELECT epoch,last_sequence,compacted_through FROM mancuerna.sync_streams WHERE account_id=?", accountID).Row().Scan(&out.Epoch, &out.LastSequence, &out.CompactedThrough)
	if err == nil && (!protocol.ValidID(out.Epoch) || out.LastSequence < 0 || out.CompactedThrough < 0 || out.CompactedThrough > out.LastSequence) {
		err = protocol.ErrCommandPersistence
	}
	return out, err
}
func (*Stream) Groups(handle shared.Transaction, accountID uuid.UUID, after int64, limit int) (out []protocol.ChangeGroup, err error) {
	defer func() {
		err = persistenceError(err)
		if err != nil {
			out = nil
		}
	}()
	if after < 0 || limit < 1 || limit > 101 {
		return nil, protocol.ErrCommandInvalid
	}
	tx, err := dataConnection(handle, accountID, true)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Raw("SELECT sequence,operation_id,changes FROM mancuerna.sync_changes WHERE account_id=? AND sequence>? ORDER BY sequence ASC LIMIT ?", accountID, after, limit).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out = make([]protocol.ChangeGroup, 0)
	storedBytes := 0
	for rows.Next() {
		var sequence int64
		var operationID uuid.UUID
		var data []byte
		if err = rows.Scan(&sequence, &operationID, &data); err != nil {
			return nil, err
		}
		// The schema caps each stored group. Keep at most one page plus the
		// whole group crossing its boundary instead of materializing 101 MiB.
		// The caller applies its canonical wire-size limit and advances only
		// through groups it actually returns, so this conservative cut is safe.
		if len(data) > protocol.PullPageBytes {
			return nil, protocol.ErrCommandPersistence
		}
		group := protocol.ChangeGroup{Sequence: strconv.FormatInt(sequence, 10), OperationID: operationID.String()}
		if json.Unmarshal(data, &group.Changes) != nil {
			return nil, protocol.ErrCommandPersistence
		}
		out = append(out, group)
		storedBytes += len(data)
		if storedBytes >= protocol.PullPageBytes {
			break
		}
	}
	return out, rows.Err()
}
