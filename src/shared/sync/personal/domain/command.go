// Package domain contains the personal synchronization protocol, not gym data
// or business rules. Account and training use cases supply validated changes.
package domain

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
)

var (
	ErrOperationMissing   = errors.New("personal operation not found")
	ErrOperationMismatch  = errors.New("personal operation content changed")
	ErrCommandInvalid     = errors.New("invalid personal command")
	ErrCommandLimit       = errors.New("personal change group exceeds its limit")
	ErrCommandScope       = errors.New("invalid personal data scope")
	ErrCommandActor       = errors.New("personal session is unavailable")
	ErrCommandOrigin      = errors.New("personal origin device is unavailable")
	ErrCommandPersistence = errors.New("personal command persistence unavailable")
)

type Actor struct{ AccountID, SessionID, DeviceID uuid.UUID }
type Base struct {
	Revision         *string `json:"revision,omitempty"`
	AfterOperationID *string `json:"after_operation_id,omitempty"`
}

// Base is a closed oneOf. An explicit null is not an absent alternative:
// accepting it would discard part of a command before hashing its retry.
func (b *Base) UnmarshalJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') || !decoder.More() {
		return ErrCommandInvalid
	}
	key, err := decoder.Token()
	if err != nil {
		return ErrCommandInvalid
	}
	var value *string
	if decoder.Decode(&value) != nil || value == nil || decoder.More() {
		return ErrCommandInvalid
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return ErrCommandInvalid
	}
	if _, err = decoder.Token(); err != io.EOF {
		return ErrCommandInvalid
	}
	next := Base{}
	switch key {
	case "revision":
		next.Revision = value
	case "after_operation_id":
		next.AfterOperationID = value
	default:
		return ErrCommandInvalid
	}
	*b = next
	return nil
}

func (b Base) Validate(operationID uuid.UUID) error {
	if (b.Revision == nil) == (b.AfterOperationID == nil) {
		return ErrCommandInvalid
	}
	if b.Revision != nil {
		_, err := ParseRevision(*b.Revision)
		return err
	}
	id, err := uuid.Parse(*b.AfterOperationID)
	if err != nil || !ValidID(id) || id.String() != *b.AfterOperationID || id == operationID {
		return ErrCommandInvalid
	}
	return nil
}
func ParseRevision(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != s {
		return 0, ErrCommandInvalid
	}
	return n, nil
}
func ValidID(id uuid.UUID) bool {
	return id != uuid.Nil && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
}
func Timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ParseCapturedAt validates client metadata without rewriting its wire text or
// making it authoritative for conflict ordering or access decisions.
func ParseCapturedAt(value string) (time.Time, error) {
	format := "2006-01-02T15:04:05Z"
	if len(value) == 24 {
		format = "2006-01-02T15:04:05.000Z"
	}
	at, err := time.Parse(format, value)
	if err != nil || at.Year() < 1 || at.Format(format) != value {
		return time.Time{}, ErrCommandInvalid
	}
	return at, nil
}

type Affected struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Revision   string `json:"revision"`
}
type FieldError struct {
	Path string `json:"path"`
	Code string `json:"code"`
}
type ResultError struct {
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	Retryable bool         `json:"retryable"`
	RequestID string       `json:"request_id"`
	Action    string       `json:"action"`
	Fields    []FieldError `json:"fields"`
}
type OperationResult struct {
	OperationID string       `json:"operation_id"`
	Outcome     string       `json:"outcome"`
	Affected    []Affected   `json:"affected,omitempty"`
	CommittedAt string       `json:"committed_at,omitempty"`
	Result      *struct{}    `json:"result,omitempty"`
	Error       *ResultError `json:"error,omitempty"`
	ConflictID  string       `json:"conflict_id,omitempty"`
}

func Failure(id uuid.UUID, outcome, code, message, action string, retryable bool) OperationResult {
	return OperationResult{OperationID: id.String(), Outcome: outcome, Error: &ResultError{Code: code, Message: message, Retryable: retryable, RequestID: id.String(), Action: action, Fields: []FieldError{}}}
}

type Receipt struct {
	OperationID, AccountID, ActorDeviceID, OriginDeviceID uuid.UUID
	RequestHash                                           [32]byte
	CommandType                                           string
	// Empty only when reading a receipt committed before migration 064.
	CapturedAt  string
	Result      OperationResult
	CommittedAt time.Time
}

func (r *Receipt) Match(hash [32]byte, commandType string, origin uuid.UUID) error {
	if r == nil || subtle.ConstantTimeCompare(r.RequestHash[:], hash[:]) != 1 || r.CommandType != commandType || r.OriginDeviceID != origin {
		return ErrOperationMismatch
	}
	return nil
}

type Change struct {
	EntityType string          `json:"entity_type"`
	EntityID   string          `json:"entity_id"`
	Revision   string          `json:"revision"`
	Data       json.RawMessage `json:"data"`
}
type Conflict struct {
	ID                            uuid.UUID
	AccountID                     uuid.UUID
	OperationID                   uuid.UUID
	EntityType                    string
	EntityID                      uuid.UUID
	BaseRevision, CurrentRevision int64
	Proposal                      json.RawMessage
	Reason                        string
	CreatedAt                     time.Time
}

func (c Conflict) Change() (Change, error) {
	view := struct {
		ID                    string          `json:"id"`
		Revision              string          `json:"revision"`
		CreatedAt             string          `json:"created_at"`
		UpdatedAt             string          `json:"updated_at"`
		DeletedAt             *string         `json:"deleted_at"`
		OperationID           string          `json:"operation_id"`
		EntityType            string          `json:"entity_type"`
		EntityID              string          `json:"entity_id"`
		BaseRevision          string          `json:"base_revision"`
		CurrentRevision       string          `json:"current_revision"`
		Proposal              json.RawMessage `json:"proposal"`
		Reason                string          `json:"reason"`
		State                 string          `json:"state"`
		ResolutionOperationID *string         `json:"resolution_operation_id"`
	}{ID: c.ID.String(), Revision: "1", CreatedAt: Timestamp(c.CreatedAt), UpdatedAt: Timestamp(c.CreatedAt), OperationID: c.OperationID.String(), EntityType: c.EntityType, EntityID: c.EntityID.String(), BaseRevision: strconv.FormatInt(c.BaseRevision, 10), CurrentRevision: strconv.FormatInt(c.CurrentRevision, 10), Proposal: c.Proposal, Reason: c.Reason, State: "open"}
	raw, err := json.Marshal(view)
	return Change{EntityType: "conflict", EntityID: c.ID.String(), Revision: "1", Data: raw}, err
}

type Canonicalizer interface{ Canonicalize([]byte) ([]byte, error) }
