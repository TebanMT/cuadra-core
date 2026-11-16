package domain

import (
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	CursorMaximumLength = 2048
	CursorListTTL       = 24 * time.Hour
	CursorSyncTTL       = 30 * 24 * time.Hour
)

var (
	ErrCursorInvalid       = errors.New("invalid personal cursor")
	ErrCursorExpired       = errors.New("personal cursor expired")
	ErrCursorEpochChanged  = errors.New("personal cursor epoch changed")
	ErrCursorConfiguration = errors.New("invalid personal cursor configuration")
)

type CursorQuery string

const (
	CursorSyncPull             CursorQuery = "sync.pull"
	CursorDeviceAuthorizations CursorQuery = "device_authorizations"
	CursorDevices              CursorQuery = "devices"
	CursorExercises            CursorQuery = "exercises"
	CursorRoutines             CursorQuery = "routines"
	CursorWorkouts             CursorQuery = "workouts"
	CursorConflicts            CursorQuery = "conflicts"
)

func (q CursorQuery) TTL() (time.Duration, error) {
	switch q {
	case CursorSyncPull:
		return CursorSyncTTL, nil
	case CursorDeviceAuthorizations, CursorDevices, CursorExercises, CursorRoutines, CursorWorkouts, CursorConflicts:
		return CursorListTTL, nil
	default:
		return 0, ErrCursorInvalid
	}
}

// CursorBinding is supplied by the authenticated use case, never copied from
// unverified cursor contents. Filters include normalized semantic defaults;
// Order is a server-defined, versioned name for a fixed keyset comparator, not
// SQL. Page size and the cursor itself are not semantic filters.
type CursorBinding struct {
	AccountID uuid.UUID
	Epoch     uuid.UUID
	Query     CursorQuery
	Filters   map[string]string
	Order     string
}

func (b CursorBinding) Validate() error {
	if !ValidID(b.AccountID) || !ValidID(b.Epoch) || len(b.Order) == 0 || len(b.Order) > 128 || len(b.Filters) > 16 {
		return ErrCursorInvalid
	}
	if _, err := b.Query.TTL(); err != nil {
		return err
	}
	for _, c := range b.Order {
		if c < '!' || c > '~' {
			return ErrCursorInvalid
		}
	}
	size := 0
	for name, value := range b.Filters {
		if len(name) == 0 || len(name) > 64 || len(value) > 512 || !utf8.ValidString(value) {
			return ErrCursorInvalid
		}
		for _, c := range name {
			if (c < 'a' || c > 'z') && c != '_' {
				return ErrCursorInvalid
			}
		}
		size += len(name) + len(value)
	}
	if size > 4096 {
		return ErrCursorInvalid
	}
	return nil
}

// CursorPosition is a tagged union selected by the binding query. Sync uses
// only Sequence, the last applied sequence, as canonical decimal BIGINT text.
// Lists use a stable Key plus the UUID tie-breaker and leave Sequence empty.
// A list ordered only by UUID puts that UUID's canonical text in Key as well.
// Repository code must bind these values as parameters to a fixed query.
type CursorPosition struct {
	Sequence string
	Key      string
	ID       uuid.UUID
}

func (p CursorPosition) Validate(query CursorQuery) error {
	if _, err := query.TTL(); err != nil {
		return err
	}
	if query == CursorSyncPull {
		n, err := strconv.ParseInt(p.Sequence, 10, 64)
		if err != nil || n < 0 || strconv.FormatInt(n, 10) != p.Sequence || p.Key != "" || p.ID != uuid.Nil {
			return ErrCursorInvalid
		}
		return nil
	}
	if p.Sequence != "" || p.Key == "" || len(p.Key) > 512 || !utf8.ValidString(p.Key) || !ValidID(p.ID) {
		return ErrCursorInvalid
	}
	return nil
}

// CursorSigner verifies position integrity, not authorization. Callers must
// revalidate the live session in their transaction, compare a verified sync
// sequence to the durable compaction floor, and reject missing pull cursors
// with ErrCursorExpired. Verify returns no position on any error.
type CursorSigner interface {
	Issue(CursorBinding, CursorPosition, time.Time) (string, error)
	Verify(string, CursorBinding, time.Time) (CursorPosition, error)
}
