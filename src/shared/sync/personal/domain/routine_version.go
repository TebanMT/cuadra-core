package domain

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const RoutineVersionMaxBytes = 1048576

var (
	ErrRoutineVersionInvalid  = errors.New("invalid personal routine version")
	ErrRoutineVersionHash     = errors.New("personal routine version hash mismatch")
	ErrRoutineVersionTooLarge = errors.New("personal routine version exceeds canonical size limit")
)

// These are closed transport shapes, not training aggregates. Publication
// authorization, unique IDs/positions and journal group limits belong to the
// producer. Dates and revision strings retain their original wire text.
type RoutineVersionDTO struct {
	ID                  string            `json:"id"`
	RoutineID           string            `json:"routine_id"`
	Revision            string            `json:"revision"`
	VersionNumber       int               `json:"version_number"`
	TitleSnapshot       string            `json:"title_snapshot"`
	FormatVersion       int               `json:"format_version"`
	SourceDraftRevision string            `json:"source_draft_revision"`
	ContentHash         string            `json:"content_hash"`
	PublishedAt         string            `json:"published_at"`
	Days                []PublishedDayDTO `json:"days"`
}
type PublishedDayDTO struct {
	ID        string                 `json:"id"`
	Label     string                 `json:"label"`
	Position  int                    `json:"position"`
	Exercises []PublishedExerciseDTO `json:"exercises"`
}
type PublishedExerciseDTO struct {
	ID               string         `json:"id"`
	ExerciseRef      ExerciseRefDTO `json:"exercise_ref"`
	NameSnapshot     string         `json:"name_snapshot"`
	LoadModeSnapshot string         `json:"load_mode_snapshot"`
	Position         int            `json:"position"`
	PrescribedSets   int            `json:"prescribed_sets"`
	PrescribedReps   string         `json:"prescribed_reps"`
	RestSeconds      *int           `json:"rest_seconds"`
	Notes            string         `json:"notes"`
}
type ExerciseRefDTO struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}

// BuildRoutineVersion accepts exactly the RoutineVersion DTO WITHOUT its
// content_hash property. It adds SHA256(canonical DTO minus only that property),
// without a prefix or additional normalization. The canonicalizer is injected
// so this protocol layer does not depend on a concrete security implementation.
func BuildRoutineVersion(rawWithoutHash []byte, canonicalizer Canonicalizer) (RoutineVersionDTO, error) {
	value, dto, err := routineVersionInput(rawWithoutHash, canonicalizer, false)
	if err != nil {
		return RoutineVersionDTO{}, err
	}
	dto.ContentHash, err = routineVersionDigest(value, canonicalizer)
	if err != nil {
		return RoutineVersionDTO{}, err
	}
	value["content_hash"] = dto.ContentHash
	if _, err = routineVersionCanonical(value, canonicalizer); err != nil {
		return RoutineVersionDTO{}, err
	}
	return dto, nil
}

// DecodeRoutineVersion validates the complete closed DTO and its digest before
// returning it. Grammar-only fixtures with illustrative hashes are not versions
// whose contents have been verified.
func DecodeRoutineVersion(rawFull []byte, canonicalizer Canonicalizer) (RoutineVersionDTO, error) {
	value, dto, err := routineVersionInput(rawFull, canonicalizer, true)
	if err != nil {
		return RoutineVersionDTO{}, err
	}
	expected, err := routineVersionDigest(value, canonicalizer)
	if err != nil {
		return RoutineVersionDTO{}, err
	}
	if subtle.ConstantTimeCompare([]byte(dto.ContentHash), []byte(expected)) != 1 {
		return RoutineVersionDTO{}, ErrRoutineVersionHash
	}
	return dto, nil
}

func routineVersionCanonical(value any, canonicalizer Canonicalizer) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, ErrRoutineVersionInvalid
	}
	canonical, err := canonicalizer.Canonicalize(raw)
	if err != nil {
		return nil, ErrRoutineVersionInvalid
	}
	if len(canonical) > RoutineVersionMaxBytes {
		return nil, ErrRoutineVersionTooLarge
	}
	return canonical, nil
}

func routineVersionDigest(value map[string]any, canonicalizer Canonicalizer) (string, error) {
	content := make(map[string]any, len(value))
	for key, field := range value {
		if key != "content_hash" {
			content[key] = field
		}
	}
	canonical, err := routineVersionCanonical(content, canonicalizer)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(canonical)
	return hex.EncodeToString(hash[:]), nil
}

func routineVersionInput(raw []byte, canonicalizer Canonicalizer, includeHash bool) (map[string]any, RoutineVersionDTO, error) {
	if canonicalizer == nil {
		return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
	}
	// S00 bounds raw HTTP requests at 2 MiB, and the canonical full DTO at 1 MiB.
	if len(raw) > 2*RoutineVersionMaxBytes {
		return nil, RoutineVersionDTO{}, ErrRoutineVersionTooLarge
	}
	// S00 canonicalization rejects duplicate keys and invalid UTF-8/UTF-16
	// escapes BEFORE encoding/json can silently overwrite/replace them.
	canonical, err := canonicalizer.Canonicalize(raw)
	if err != nil {
		return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
	}
	if len(canonical) > RoutineVersionMaxBytes {
		return nil, RoutineVersionDTO{}, ErrRoutineVersionTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	var parsed any
	if decoder.Decode(&parsed) != nil {
		return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
	}
	fields := []string{"id", "routine_id", "revision", "version_number", "title_snapshot", "format_version", "source_draft_revision", "published_at", "days"}
	if includeHash {
		fields = append(fields, "content_hash")
	}
	value, ok := routineObject(parsed, fields...)
	if !ok || !routineID(value["id"]) || !routineID(value["routine_id"]) || value["revision"] != "1" ||
		!routineInt(value["version_number"], 1, 2147483647) || !routineString(value["title_snapshot"], 1, 120) ||
		!routineInt(value["format_version"], 1, 1) || !routineRevision(value["source_draft_revision"]) || !routineTimestamp(value["published_at"]) {
		return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
	}
	if includeHash {
		hash, ok := value["content_hash"].(string)
		if !ok || len(hash) != 64 {
			return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
		}
		decoded, err := hex.DecodeString(hash)
		if err != nil || hex.EncodeToString(decoded) != hash {
			return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
		}
	}
	days, ok := value["days"].([]any)
	if !ok || len(days) < 1 || len(days) > 14 {
		return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
	}
	for _, item := range days {
		day, ok := routineObject(item, "id", "label", "position", "exercises")
		if !ok || !routineID(day["id"]) || !routineString(day["label"], 1, 80) || !routineInt(day["position"], 0, 13) {
			return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
		}
		exercises, ok := day["exercises"].([]any)
		if !ok || len(exercises) < 1 || len(exercises) > 50 {
			return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
		}
		for _, item := range exercises {
			exercise, ok := routineObject(item, "id", "exercise_ref", "name_snapshot", "load_mode_snapshot", "position", "prescribed_sets", "prescribed_reps", "rest_seconds", "notes")
			if !ok || !routineID(exercise["id"]) || !routineString(exercise["name_snapshot"], 1, 120) ||
				(exercise["load_mode_snapshot"] != "external" && exercise["load_mode_snapshot"] != "none") ||
				!routineInt(exercise["position"], 0, 49) || !routineInt(exercise["prescribed_sets"], 1, 100) ||
				!routineString(exercise["prescribed_reps"], 1, 80) || !routineString(exercise["notes"], 0, 1000) ||
				(exercise["rest_seconds"] != nil && !routineInt(exercise["rest_seconds"], 0, 3600)) {
				return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
			}
			ref, ok := routineObject(exercise["exercise_ref"], "source", "id")
			if !ok || !routineID(ref["id"]) || (ref["source"] != "catalog" && ref["source"] != "personal") {
				return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
			}
		}
	}
	var dto RoutineVersionDTO
	if json.Unmarshal(canonical, &dto) != nil {
		return nil, RoutineVersionDTO{}, ErrRoutineVersionInvalid
	}
	return value, dto, nil
}

func routineObject(value any, fields ...string) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != len(fields) {
		return nil, false
	}
	for _, field := range fields {
		if _, exists := object[field]; !exists {
			return nil, false
		}
	}
	return object, true
}
func routineID(value any) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	id, err := uuid.Parse(s)
	return err == nil && ValidID(id) && id.String() == s
}
func routineString(value any, min, max int) bool {
	s, ok := value.(string)
	return ok && utf8.ValidString(s) && utf8.RuneCountInString(s) >= min && utf8.RuneCountInString(s) <= max
}
func routineInt(value any, min, max int64) bool {
	n, ok := value.(json.Number)
	if !ok {
		return false
	}
	v, err := strconv.ParseInt(string(n), 10, 64)
	return err == nil && v >= min && v <= max
}
func routineRevision(value any) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	_, err := ParseRevision(s)
	return err == nil
}
func routineTimestamp(value any) bool {
	s, ok := value.(string)
	if !ok || (len(s) != 20 && len(s) != 24) {
		return false
	}
	layout := "2006-01-02T15:04:05Z"
	if len(s) == 24 {
		layout = "2006-01-02T15:04:05.000Z"
	}
	at, err := time.Parse(layout, s)
	return err == nil && at.Year() >= 1 && at.Format(layout) == s
}
