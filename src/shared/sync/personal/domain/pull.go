package domain

import "github.com/google/uuid"

const PullPageBytes = 1048576

type StreamState struct {
	Epoch                          uuid.UUID
	LastSequence, CompactedThrough int64
}

func PullBinding(accountID, epoch uuid.UUID) CursorBinding {
	return CursorBinding{AccountID: accountID, Epoch: epoch, Query: CursorSyncPull, Filters: map[string]string{}, Order: "sequence.asc.v1"}
}

type ChangeGroup struct {
	Sequence    string   `json:"sequence"`
	OperationID string   `json:"operation_id"`
	Changes     []Change `json:"changes"`
}

type PullResult struct {
	ProtocolVersion int           `json:"protocol_version"`
	Epoch           string        `json:"epoch"`
	Groups          []ChangeGroup `json:"groups"`
	NextCursor      string        `json:"next_cursor"`
	HasMore         bool          `json:"has_more"`
}
