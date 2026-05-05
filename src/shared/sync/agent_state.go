//go:build sidecar

package sync

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// sync_state keys (free-form, kept here as constants so the agent and
// callers stay in lockstep).
const (
	keyClientID               = "client_id"
	keyLastPulledAt           = "last_pulled_at_ms"
	keyLastSyncedAt           = "last_synced_at_ms"
	keyInitialSyncCompletedAt = "initial_sync_completed_at_ms"
	keyFullSyncCursor         = "full_sync_cursor"
	keyLastError              = "last_error"
	keyConsecutiveFailures    = "consecutive_failures"
	keyNextRetryAt            = "next_retry_at_ms"
	keySidecarToken           = "sidecar_token"
)

// State is the in-memory projection of sync_state. Use ReadState / WriteState
// for atomic round-trips; individual setters short-circuit unrelated keys.
type State struct {
	ClientID               uuid.UUID
	LastPulledAt           time.Time
	LastSyncedAt           time.Time
	InitialSyncCompletedAt time.Time
	FullSyncCursor         string
	LastError              string
	ConsecutiveFailures    int
	NextRetryAt            time.Time
	SidecarToken           string
}

// ReadState — fetches all known sync_state rows for this device. Missing
// keys leave the corresponding field at zero value.
func ReadState(ctx context.Context, tx sharedDomain.Transaction) (State, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	rows := []struct {
		Key   string `db:"key"`
		Value string `db:"value"`
	}{}
	if err := stx.Select(ctx, &rows, `SELECT key, value FROM sync_state`); err != nil {
		return State{}, err
	}
	s := State{}
	for _, r := range rows {
		switch r.Key {
		case keyClientID:
			if id, err := uuid.Parse(r.Value); err == nil {
				s.ClientID = id
			}
		case keyLastPulledAt:
			s.LastPulledAt = parseMs(r.Value)
		case keyLastSyncedAt:
			s.LastSyncedAt = parseMs(r.Value)
		case keyInitialSyncCompletedAt:
			s.InitialSyncCompletedAt = parseMs(r.Value)
		case keyFullSyncCursor:
			s.FullSyncCursor = r.Value
		case keyLastError:
			s.LastError = r.Value
		case keyConsecutiveFailures:
			if n, err := strconv.Atoi(r.Value); err == nil {
				s.ConsecutiveFailures = n
			}
		case keyNextRetryAt:
			s.NextRetryAt = parseMs(r.Value)
		case keySidecarToken:
			s.SidecarToken = r.Value
		}
	}
	return s, nil
}

// SetKV — write-through key/value setter. Used both as building block and
// directly by callers that only touch one key.
func SetKV(ctx context.Context, tx sharedDomain.Transaction, key, value string) error {
	stx := tx.(*sharedDomain.SqlxTransaction)
	_, err := stx.Exec(ctx, `
		INSERT INTO sync_state(key, value, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().UnixMilli(),
	)
	return err
}

// EnsureClientID — idempotently materialises a stable client_id (UUID v4)
// in sync_state on first call. Returns the stored or freshly-generated id.
func EnsureClientID(ctx context.Context, tx sharedDomain.Transaction) (uuid.UUID, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var existing sql.NullString
	err := stx.Get(ctx, &existing, `SELECT value FROM sync_state WHERE key = ?`, keyClientID)
	if err == nil && existing.Valid {
		if id, err := uuid.Parse(existing.String); err == nil {
			return id, nil
		}
	}
	if err != nil && err != sql.ErrNoRows {
		return uuid.Nil, err
	}
	id := uuid.New()
	if err := SetKV(ctx, tx, keyClientID, id.String()); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// SetLastPulledAt — convenience wrapper. Stored as epoch ms for SQLite
// time-discipline (ADR-002 §4 — no SQLite TIMESTAMPTZ).
func SetLastPulledAt(ctx context.Context, tx sharedDomain.Transaction, t time.Time) error {
	return SetKV(ctx, tx, keyLastPulledAt, strconv.FormatInt(t.UnixMilli(), 10))
}

func SetLastSyncedAt(ctx context.Context, tx sharedDomain.Transaction, t time.Time) error {
	return SetKV(ctx, tx, keyLastSyncedAt, strconv.FormatInt(t.UnixMilli(), 10))
}

func SetInitialSyncCompletedAt(ctx context.Context, tx sharedDomain.Transaction, t time.Time) error {
	return SetKV(ctx, tx, keyInitialSyncCompletedAt, strconv.FormatInt(t.UnixMilli(), 10))
}

func SetFullSyncCursor(ctx context.Context, tx sharedDomain.Transaction, cursor string) error {
	return SetKV(ctx, tx, keyFullSyncCursor, cursor)
}

func ClearFullSyncCursor(ctx context.Context, tx sharedDomain.Transaction) error {
	return SetKV(ctx, tx, keyFullSyncCursor, "")
}

func SetLastError(ctx context.Context, tx sharedDomain.Transaction, msg string) error {
	return SetKV(ctx, tx, keyLastError, msg)
}

func ClearLastError(ctx context.Context, tx sharedDomain.Transaction) error {
	return SetKV(ctx, tx, keyLastError, "")
}

func SetConsecutiveFailures(ctx context.Context, tx sharedDomain.Transaction, n int) error {
	return SetKV(ctx, tx, keyConsecutiveFailures, strconv.Itoa(n))
}

func SetNextRetryAt(ctx context.Context, tx sharedDomain.Transaction, t time.Time) error {
	return SetKV(ctx, tx, keyNextRetryAt, strconv.FormatInt(t.UnixMilli(), 10))
}

// SetSidecarToken persists the long-lived sidecar credential (sk_live_…)
// minted by the cloud on operator login. The agent rereads it from
// sync_state on its AgentReload callback so the next sync tick picks up
// the new credential without restarting the binary.
func SetSidecarToken(ctx context.Context, tx sharedDomain.Transaction, token string) error {
	return SetKV(ctx, tx, keySidecarToken, token)
}

func parseMs(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}
	}
	return time.UnixMilli(n).UTC()
}
