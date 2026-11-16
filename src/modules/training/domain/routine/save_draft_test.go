package routine

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

func TestDraftSaveCASAndReopenRetainIdentityWithoutPublication(t *testing.T) {
	now := time.Date(2026, 10, 19, 2, 0, 0, 0, time.UTC)
	root, current, err := NewDraftRoutine(uuid.New(), uuid.New(), incompleteDraft(), now)
	if err != nil {
		t.Fatal(err)
	}
	rootBefore := *root
	before, _ := json.Marshal(current)
	next := incompleteDraft()
	next.Title = "  Borrador Élite  "
	if err = current.Save(next, nil, 2, now.Add(time.Second)); !errors.Is(err, trainingErrors.ErrRoutineRevisionConflict) {
		t.Fatal("stale CAS accepted", err)
	}
	after, _ := json.Marshal(current)
	if string(before) != string(after) {
		t.Fatal("failed save mutated draft")
	}
	base := uuid.New()
	if err = current.Save(next, &base, 1, now.Add(time.Second+1234567)); err != nil {
		t.Fatal(err)
	}
	if current.Revision != 2 || current.Content.Title != next.Title || current.BaseVersionID == nil || *current.BaseVersionID != base || !current.CreatedAt.Equal(now) || current.UpdatedAt.Nanosecond() != 1000000 || !reflect.DeepEqual(rootBefore, *root) {
		t.Fatal("save changed root or identity")
	}
	next.Days[0].Label = "external change"
	base = uuid.New()
	if current.Content.Days[0].Label != "" || *current.BaseVersionID == base {
		t.Fatal("save aliased input")
	}
	deleted := now.Add(2 * time.Second)
	current.Content, current.DeletedAt, current.UpdatedAt, current.Revision = nil, &deleted, deleted, 3
	if err = current.Save(incompleteDraft(), nil, 3, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if current.Revision != 4 || current.DeletedAt != nil || current.Content == nil || !current.CreatedAt.Equal(now) || !reflect.DeepEqual(rootBefore, *root) {
		t.Fatal("reopening reset revision or published")
	}
	if err = current.ValidateSave(root.AccountID, 3); err != nil {
		t.Fatal(err)
	}
	if current.ValidateSave(uuid.New(), 3) == nil || current.ValidateSave(root.AccountID, 2) == nil {
		t.Fatal("invalid storage binding accepted")
	}
}

func TestDraftSaveCommandClosedShapeAndPreservedCapture(t *testing.T) {
	rev := "1"
	c := SaveDraftCommand{OperationID: uuid.NewString(), OriginDeviceID: uuid.NewString(), Type: SaveDraftCommandType, EntityID: uuid.NewString(), Base: protocol.Base{Revision: &rev}, CapturedAt: "2026-10-19T00:00:00Z", Payload: SaveDraftPayload{Content: incompleteDraft()}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SaveDraftCommand
	if json.Unmarshal(raw, &decoded) != nil || !reflect.DeepEqual(c, decoded) {
		t.Fatal("valid request changed")
	}
	for _, edit := range []func(string) string{
		func(s string) string { return strings.Replace(s, `,"base_version_id":null`, "", 1) },
		func(s string) string {
			return strings.Replace(s, `"base_version_id":null`, `"base_version_id":false`, 1)
		},
		func(s string) string { return strings.Replace(s, `"base_version_id":null`, `"base_version_id":""`, 1) },
		func(s string) string {
			return strings.Replace(s, `"base_version_id":null`, `"base_version_id":null,"base_version_id":null`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"base_version_id":null`, `"base_version_id":null,"unknown":1`, 1)
		},
		func(s string) string { return strings.Replace(s, `"title":""`, `"title":null`, 1) },
		func(s string) string { return strings.Replace(s, `"title":""`, `"title":"\ud800"`, 1) },
		func(s string) string {
			return strings.Replace(s, `"2026-10-19T00:00:00Z"`, `"2026-10-19T00:00:00.0Z"`, 1)
		},
	} {
		if json.Unmarshal([]byte(edit(string(raw))), &decoded) == nil {
			t.Fatal("open or invalid save command accepted")
		}
	}
	c.Base = protocol.Base{AfterOperationID: &c.OperationID}
	if c.ValidateEnvelope() == nil {
		t.Fatal("self dependency accepted")
	}
}
