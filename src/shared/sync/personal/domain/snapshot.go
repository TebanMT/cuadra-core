package domain

import (
	"errors"
	"github.com/google/uuid"
	"time"
)

const SnapshotTTL = 24 * time.Hour
const SnapshotPageRecords = 1000
const SnapshotMaxPages = 100000

var (
	ErrSnapshotMissing  = errors.New("personal snapshot not found")
	ErrSnapshotExpired  = errors.New("personal snapshot expired")
	ErrSnapshotConflict = errors.New("personal snapshot ID unavailable")
)

type Capabilities struct {
	ProtocolVersion        int      `json:"protocol_version"`
	MinimumProtocolVersion int      `json:"minimum_protocol_version"`
	MinimumAppVersion      string   `json:"minimum_app_version"`
	MaxPushOperations      int      `json:"max_push_operations"`
	MaxRequestBytes        int      `json:"max_request_bytes"`
	MaxGroupBytes          int      `json:"max_group_bytes"`
	Features               []string `json:"features"`
	PolicyVersion          *string  `json:"policy_version"`
}
type Snapshot struct {
	ID              string       `json:"id"`
	Epoch           string       `json:"epoch"`
	ThroughSequence string       `json:"through_sequence"`
	PageCount       int          `json:"page_count"`
	PageHashes      []string     `json:"page_hashes"`
	ExpiresAt       string       `json:"expires_at"`
	Cursor          string       `json:"cursor"`
	Capabilities    Capabilities `json:"capabilities"`
}
type SnapshotPage struct {
	SnapshotID string   `json:"snapshot_id"`
	PageNumber int      `json:"page_number"`
	Hash       string   `json:"hash"`
	Records    []Change `json:"records"`
}

// Storage metadata is kept distinct from the immutable public manifest.
type StoredSnapshot struct {
	ID, AccountID, Epoch uuid.UUID
	ThroughSequence      int64
	CreatedAt, ExpiresAt time.Time
	Manifest             Snapshot
}
