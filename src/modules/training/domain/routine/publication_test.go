package routine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

func publicationFixture(t *testing.T) (*Routine, *DraftRecord, PublishPayload, PublicationMaterial, time.Time) {
	t.Helper()
	content := Draft{FormatVersion: 1, Title: "  Fuerza Élite <A>  ", Days: []DraftDay{}}
	payload := PublishPayload{DraftRevision: "1", VersionID: uuid.NewString(), Days: []DayIDMap{}}
	material := PublicationMaterial{NextVersionNumber: 1, Exercises: map[string]ExerciseSnapshot{}}
	for i := 0; i < 2; i++ {
		day := DraftDay{ID: uuid.NewString(), Label: " Día ", Exercises: []DraftExercise{}}
		m := DayIDMap{DraftDayID: day.ID, ID: uuid.NewString(), Exercises: []ExerciseIDMap{}}
		for j := 0; j < 2; j++ {
			sets, rest := 3, 0
			ref := ExerciseRef{Source: "personal", ID: uuid.NewString()}
			e := DraftExercise{ID: uuid.NewString(), ExerciseRef: &ref, PrescribedSets: &sets, PrescribedReps: " 8–12 ", RestSeconds: &rest, Notes: "note"}
			day.Exercises = append(day.Exercises, e)
			m.Exercises = append(m.Exercises, ExerciseIDMap{DraftExerciseID: e.ID, ID: uuid.NewString()})
			material.Exercises[e.ID] = ExerciseSnapshot{Reference: ref, Name: " Press ", LoadMode: "external"}
		}
		content.Days = append(content.Days, day)
		payload.Days = append(payload.Days, m)
	}
	now := time.Date(2026, 10, 19, 3, 0, 0, 123000000, time.UTC)
	r, d, err := NewDraftRoutine(uuid.New(), uuid.New(), content, now)
	if err != nil {
		t.Fatal(err)
	}
	return r, d, payload, material, now.Add(time.Second)
}

func TestRoutinePublicationUsesDraftOrderExactHashAndGrowingTombstone(t *testing.T) {
	r, d, p, m, now := publicationFixture(t)
	originalRoot, originalDraft := *r, d.Content.Clone()
	originalMaps := append([]DayIDMap(nil), p.Days...)
	for i := range originalMaps {
		originalMaps[i].Exercises = append([]ExerciseIDMap(nil), originalMaps[i].Exercises...)
	}
	// The payload is a map, not an alternative order of the edited content.
	p.Days[0], p.Days[1] = p.Days[1], p.Days[0]
	p.Days[0].Exercises[0], p.Days[0].Exercises[1] = p.Days[0].Exercises[1], p.Days[0].Exercises[0]
	actor := uuid.New()
	out, err := NewPublication(r, d, p, m, actor, now, crypto.JSONCanonicalizer{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Root.Title != r.Title || out.Root.Revision != 2 || out.Root.Status != "ready" || out.Draft.Revision != 2 || out.Draft.Content != nil || out.Draft.DeletedAt == nil || out.Draft.BaseVersionID == nil || *out.Draft.BaseVersionID != out.Version.ID || out.Version.SourceDraftRevision != 1 || out.Version.ActorDeviceID != actor {
		t.Fatal("publication did not atomically advance aggregates")
	}
	for i, day := range out.Version.Days {
		if day.ID.String() != originalMaps[i].ID || day.Position != i || day.Label != d.Content.Days[i].Label {
			t.Fatal("map order replaced editorial order")
		}
		for j, e := range day.Exercises {
			if e.ID.String() != originalMaps[i].Exercises[j].ID || e.Position != j || e.PrescribedReps != " 8–12 " || e.NameSnapshot != " Press " {
				t.Fatal("published content normalized or reordered")
			}
		}
	}
	if !reflect.DeepEqual(*r, originalRoot) || !reflect.DeepEqual(*d.Content, originalDraft) || d.Revision != 1 {
		t.Fatal("constructor mutated source")
	}
	raw, _ := json.Marshal(out.Version.DTO())
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		t.Fatal("invalid DTO")
	}
	delete(fields, "content_hash")
	without, _ := json.Marshal(fields)
	canonical, err := (crypto.JSONCanonicalizer{}).Canonicalize(without)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	if digest != out.Version.ContentHash {
		t.Fatal("version hash material differs")
	}
	if dto, err := protocol.DecodeRoutineVersion(raw, crypto.JSONCanonicalizer{}); err != nil || dto.ContentHash != hex.EncodeToString(digest[:]) {
		t.Fatal("producer incompatible with shared decoder", err)
	}
	*d.Content.Days[0].Exercises[0].RestSeconds = 55
	if *out.Version.Days[0].Exercises[0].RestSeconds != 0 {
		t.Fatal("immutable version aliases editable input")
	}
}

func TestRoutinePublicationRejectsIncompleteContentAndNonBijectiveMaps(t *testing.T) {
	for _, edit := range []func(*Routine, *DraftRecord, *PublishPayload, *PublicationMaterial){
		func(_ *Routine, d *DraftRecord, _ *PublishPayload, _ *PublicationMaterial) { d.Content.Title = "  " },
		func(_ *Routine, d *DraftRecord, _ *PublishPayload, _ *PublicationMaterial) {
			d.Content.Days[0].Label = ""
		},
		func(_ *Routine, d *DraftRecord, _ *PublishPayload, _ *PublicationMaterial) {
			d.Content.Days[0].Exercises = []DraftExercise{}
		},
		func(_ *Routine, d *DraftRecord, _ *PublishPayload, _ *PublicationMaterial) {
			d.Content.Days[0].Exercises[0].PrescribedSets = nil
		},
		func(_ *Routine, d *DraftRecord, _ *PublishPayload, _ *PublicationMaterial) {
			d.Content.Days[0].Exercises[0].PrescribedReps = "\n "
		},
		func(_ *Routine, d *DraftRecord, _ *PublishPayload, _ *PublicationMaterial) {
			d.Content.Days[0].Exercises[0].ExerciseRef = nil
		},
		func(_ *Routine, _ *DraftRecord, p *PublishPayload, _ *PublicationMaterial) { p.Days = p.Days[:1] },
		func(_ *Routine, _ *DraftRecord, p *PublishPayload, _ *PublicationMaterial) {
			p.Days[0].DraftDayID = uuid.NewString()
		},
		func(_ *Routine, _ *DraftRecord, p *PublishPayload, _ *PublicationMaterial) {
			p.Days[1].DraftDayID = p.Days[0].DraftDayID
		},
		func(_ *Routine, _ *DraftRecord, p *PublishPayload, _ *PublicationMaterial) {
			p.Days[1].ID = p.Days[0].ID
		},
		func(_ *Routine, _ *DraftRecord, p *PublishPayload, _ *PublicationMaterial) {
			p.Days[0].Exercises[0].ID = p.VersionID
		},
		func(_ *Routine, _ *DraftRecord, p *PublishPayload, _ *PublicationMaterial) {
			p.Days[0].Exercises[1].DraftExerciseID = p.Days[0].Exercises[0].DraftExerciseID
		},
		func(_ *Routine, d *DraftRecord, p *PublishPayload, _ *PublicationMaterial) {
			p.Days[0].ID = d.Content.Days[1].Exercises[0].ID
		},
		func(_ *Routine, d *DraftRecord, _ *PublishPayload, m *PublicationMaterial) {
			delete(m.Exercises, d.Content.Days[0].Exercises[0].ID)
		},
		func(r *Routine, _ *DraftRecord, _ *PublishPayload, _ *PublicationMaterial) { r.Status = "archived" },
	} {
		r, d, p, m, now := publicationFixture(t)
		edit(r, d, &p, &m)
		if _, err := NewPublication(r, d, p, m, uuid.New(), now, crypto.JSONCanonicalizer{}); err == nil {
			t.Fatal("incomplete or ambiguous publication accepted")
		}
	}
	r, d, p, m, now := publicationFixture(t)
	p.DraftRevision = "2"
	if _, err := NewPublication(r, d, p, m, uuid.New(), now, crypto.JSONCanonicalizer{}); !errors.Is(err, trainingErrors.ErrRoutineRevisionConflict) {
		t.Fatal("draft CAS ignored", err)
	}
	p.DraftRevision = "1"
	m.NextVersionNumber = math.MaxInt32 + 1
	if _, err := NewPublication(r, d, p, m, uuid.New(), now, crypto.JSONCanonicalizer{}); !errors.Is(err, trainingErrors.ErrTrainingLimit) {
		t.Fatal("version counter overflow accepted", err)
	}
}

func TestRoutinePublicationClosedCommandAndUnmodifiedRequest(t *testing.T) {
	r, _, p, _, _ := publicationFixture(t)
	revision := "1"
	c := PublishCommand{OperationID: uuid.NewString(), OriginDeviceID: uuid.NewString(), Type: PublishCommandType, EntityID: r.ID.String(), Base: protocol.Base{Revision: &revision}, CapturedAt: "2026-10-19T00:00:00Z", Payload: p}
	raw, _ := json.Marshal(c)
	var decoded PublishCommand
	if json.Unmarshal(raw, &decoded) != nil || !reflect.DeepEqual(decoded, c) {
		t.Fatal("valid publish command changed")
	}
	for _, edit := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"draft_revision":"1"`, `"draft_revision":1`, 1) },
		func(s string) string {
			return strings.Replace(s, `"draft_revision":"1"`, `"draft_revision":"1","extra":0`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"draft_revision":"1"`, `"draft_revision":"1","draft_revision":"1"`, 1)
		},
		func(s string) string { return strings.Replace(s, `"exercises":[`, `"unknown":null,"exercises":[`, 1) },
		func(s string) string { return strings.Replace(s, p.Days[0].Exercises[0].ID, "not-an-id", 1) },
	} {
		if json.Unmarshal([]byte(edit(string(raw))), &decoded) == nil {
			t.Fatal("open publish command accepted")
		}
	}
}

func TestRoutinePublicationHashUsesCanonicalRatherThanHTMLEscapedSize(t *testing.T) {
	r, d, p, m, now := publicationFixture(t)
	d.Content.Days = []DraftDay{}
	p.Days = []DayIDMap{}
	m.Exercises = map[string]ExerciseSnapshot{}
	for i := 0; i < 14; i++ {
		day := DraftDay{ID: uuid.NewString(), Label: "Día", Exercises: []DraftExercise{}}
		mapping := DayIDMap{DraftDayID: day.ID, ID: uuid.NewString(), Exercises: []ExerciseIDMap{}}
		for j := 0; j < 50; j++ {
			sets := 1
			ref := ExerciseRef{Source: "personal", ID: uuid.NewString()}
			e := DraftExercise{ID: uuid.NewString(), ExerciseRef: &ref, PrescribedSets: &sets, PrescribedReps: "1", Notes: strings.Repeat("<", 900)}
			day.Exercises = append(day.Exercises, e)
			mapping.Exercises = append(mapping.Exercises, ExerciseIDMap{DraftExerciseID: e.ID, ID: uuid.NewString()})
			m.Exercises[e.ID] = ExerciseSnapshot{Reference: ref, Name: "Press", LoadMode: "external"}
		}
		d.Content.Days = append(d.Content.Days, day)
		p.Days = append(p.Days, mapping)
	}
	out, err := NewPublication(r, d, p, m, uuid.New(), now, crypto.JSONCanonicalizer{})
	if err != nil {
		t.Fatal("internal HTML escaping applied raw HTTP bound", err)
	}
	raw, _ := json.Marshal(out.Version.DTO())
	canonical, err := (crypto.JSONCanonicalizer{}).Canonicalize(raw)
	if err != nil || len(raw) <= 2*1048576 || len(canonical) >= 1048576 {
		t.Fatal("fixture did not cross internal raw bound", err)
	}
}
