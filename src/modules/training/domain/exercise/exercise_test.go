package exercise

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/google/uuid"
)

func commandJSON() string {
	return `{"operation_id":"11111111-1111-4111-8111-111111111111","origin_device_id":"22222222-2222-4222-8222-222222222222","type":"personal_exercise.create","entity_id":"33333333-3333-4333-8333-333333333333","base":{"revision":"0"},"captured_at":"2026-10-19T10:00:00Z","payload":{"name":"  Press Élite 漢  ","load_mode":"external","notes":""}}`
}

func TestPersonalExerciseCreateCommandClosedContractAndOriginalStrings(t *testing.T) {
	raw := commandJSON()
	var c CreateCommand
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if c.Payload.Name != "  Press Élite 漢  " || c.CapturedAt != "2026-10-19T10:00:00Z" || c.Payload.Notes != "" {
		t.Fatal("command was normalized")
	}
	for _, malformed := range []string{
		strings.Replace(raw, `"notes":""`, `"notes":null`, 1),
		strings.Replace(raw, `,"notes":""`, "", 1),
		strings.Replace(raw, `"notes":""`, `"notes":"","notes":""`, 1),
		strings.Replace(raw, `"notes":""`, `"notes":"","extra":true`, 1),
		strings.Replace(raw, `"payload":`, `"extra":0,"payload":`, 1),
		strings.Replace(raw, `"base":{"revision":"0"}`, `"base":{"revision":"0","after_operation_id":null}`, 1),
		strings.Replace(raw, `"captured_at":"2026-10-19T10:00:00Z"`, `"captured_at":"2026-02-30T10:00:00Z"`, 1),
		strings.Replace(raw, `"captured_at":"2026-10-19T10:00:00Z"`, `"captured_at":"2026-10-19T10:00:00.00Z"`, 1),
		strings.Replace(raw, `"notes":""`, `"notes":"\ud800"`, 1),
		strings.Replace(raw, `"notes":""`, `"notes":"\udc00"`, 1),
		strings.Replace(raw, `"notes":""`, `"notes":"\u0000"`, 1),
		raw + `{}`,
	} {
		if err := json.Unmarshal([]byte(malformed), &c); err == nil {
			t.Fatalf("invalid closed command accepted: %v", err)
		}
	}
	if err := json.Unmarshal([]byte(strings.Replace(raw, `"notes":""`, `"notes":"\ud83d\ude00"`, 1)), &c); err != nil || c.Payload.Notes != "😀" {
		t.Fatal("valid Unicode pair rejected", err)
	}
}

func TestPersonalExercisePreservesIdentityValuesAndCreationInvariants(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 0, 0, 123456789, time.FixedZone("synthetic", 3600))
	id, owner := uuid.New(), uuid.New()
	e, err := NewPersonal(id, owner, "  Press Élite 漢  ", External, "Nota\nsegunda", now)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != id || e.AccountID != owner || e.Name != "  Press Élite 漢  " || e.Revision != 1 || e.CreatedAt.Location() != time.UTC || e.CreatedAt.Nanosecond() != 123000000 || e.ValidateCreation(owner) != nil {
		t.Fatal("creation changed identity/content")
	}
	if e.ValidateCreation(uuid.New()) == nil {
		t.Fatal("foreign owner accepted")
	}
	e.Revision = 2
	if e.ValidateCreation(owner) == nil {
		t.Fatal("existing revision accepted as create")
	}
	for _, name := range []string{"", " \n ", strings.Repeat("漢", 121), "a\x00b", string([]byte{0xff})} {
		if _, err := NewPersonal(uuid.New(), owner, name, None, "", now); !errors.Is(err, trainingErrors.ErrInvalidExercise) {
			t.Fatal("invalid name accepted")
		}
	}
	if _, err := NewPersonal(uuid.New(), owner, strings.Repeat("漢", 120), None, strings.Repeat("😀", 1000), now); err != nil {
		t.Fatal("Unicode limit counted bytes", err)
	}
}
