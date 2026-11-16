package workout

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestOfflineCommandClosedUnionPreservesCaptureAndRejectsAmbiguity(t *testing.T) {
	_, start, _ := startFixture()
	_, set, _ := setDomainFixture(t)
	for _, value := range []OfflineCommand{{Start: &start}, {CreateSet: &set}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var roundtrip OfflineCommand
		if json.Unmarshal(raw, &roundtrip) != nil || !reflect.DeepEqual(value, roundtrip) || roundtrip.OperationID() != value.OperationID() || roundtrip.OriginDeviceID() != value.OriginDeviceID() {
			t.Fatal("union changed operation")
		}
		for _, bad := range []string{
			strings.Replace(string(raw), `"type":`, `"Type":`, 1),
			strings.Replace(string(raw), `"type":`, `"extra":true,"type":`, 1),
			strings.Replace(string(raw), `"type":`, `"type":"workout.complete","type":`, 1),
			strings.ReplaceAll(string(raw), valueType(value), "workout.complete"),
			`null`, `[]`,
		} {
			if json.Unmarshal([]byte(bad), &roundtrip) == nil || !reflect.DeepEqual(value, roundtrip) {
				t.Fatal("malformed union accepted or replaced previous value")
			}
		}
	}
	for _, bad := range []OfflineCommand{{}, {Start: &start, CreateSet: &set}} {
		if _, err := json.Marshal(bad); err == nil || bad.ValidateEnvelope() == nil {
			t.Fatal("ambiguous union accepted")
		}
	}
}
func valueType(c OfflineCommand) string {
	if c.Start != nil {
		return StartCommandType
	}
	return CreateSetCommandType
}
