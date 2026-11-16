package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCursorDomainPositionDiscriminatesSyncAndStableLists(t *testing.T) {
	for _, sequence := range []string{"0", "9007199254740993", "9223372036854775807"} {
		if err := (CursorPosition{Sequence: sequence}).Validate(CursorSyncPull); err != nil {
			t.Fatal(err)
		}
	}
	for _, sequence := range []string{"", "-1", "+1", "01", "1.0", "1e3", "9223372036854775808", " 1"} {
		if err := (CursorPosition{Sequence: sequence}).Validate(CursorSyncPull); !errors.Is(err, ErrCursorInvalid) {
			t.Fatal("noncanonical or out-of-range sequence accepted")
		}
	}
	id := uuid.MustParse("87654321-4321-4321-8321-123456789abc")
	for _, position := range []CursorPosition{{Sequence: "0", Key: "x"}, {Sequence: "0", ID: id}} {
		if position.Validate(CursorSyncPull) == nil {
			t.Fatal("list position accepted as sync")
		}
	}
	if (CursorPosition{Key: id.String(), ID: id}).Validate(CursorDevices) != nil {
		t.Fatal("UUID-only ordering representation rejected")
	}
	for _, position := range []CursorPosition{{ID: id}, {Key: "a"}, {Key: "a", ID: id, Sequence: "0"}, {Key: strings.Repeat("a", 513), ID: id}, {Key: "\xff", ID: id}} {
		if position.Validate(CursorDevices) == nil {
			t.Fatal("invalid list keyset accepted")
		}
	}
}

func TestCursorDomainBindingRequiresOwnerEpochKnownQueryAndBoundedSemanticInput(t *testing.T) {
	b := CursorBinding{AccountID: uuid.New(), Epoch: uuid.New(), Query: CursorExercises, Order: "name.asc-id.asc.v1", Filters: map[string]string{"query": "sentadilla"}}
	if b.Validate() != nil {
		t.Fatal("valid binding rejected")
	}
	for _, change := range []func(*CursorBinding){
		func(b *CursorBinding) { b.AccountID = uuid.Nil },
		func(b *CursorBinding) { b.Epoch = uuid.Nil },
		func(b *CursorBinding) { b.Query = "users" },
		func(b *CursorBinding) { b.Order = "" },
		func(b *CursorBinding) { b.Order = "created_at\nasc" },
		func(b *CursorBinding) { b.Order = strings.Repeat("a", 129) },
		func(b *CursorBinding) { b.Filters = map[string]string{"Query": "a"} },
		func(b *CursorBinding) { b.Filters = map[string]string{"query": "\xff"} },
		func(b *CursorBinding) { b.Filters = map[string]string{"query": strings.Repeat("a", 513)} },
		func(b *CursorBinding) {
			b.Filters = map[string]string{}
			for i := 0; i < 17; i++ {
				b.Filters[string(rune('a'+i))] = ""
			}
		},
		func(b *CursorBinding) {
			b.Filters = map[string]string{}
			for i := 0; i < 9; i++ {
				b.Filters[string(rune('a'+i))] = strings.Repeat("a", 512)
			}
		},
	} {
		changed := b
		change(&changed)
		if changed.Validate() == nil {
			t.Fatal("invalid binding accepted")
		}
	}
	for _, query := range []CursorQuery{CursorDevices, CursorDeviceAuthorizations, CursorExercises, CursorRoutines, CursorWorkouts, CursorConflicts} {
		if ttl, err := query.TTL(); err != nil || ttl != CursorListTTL {
			t.Fatal("list TTL changed")
		}
	}
	if ttl, err := CursorSyncPull.TTL(); err != nil || ttl != CursorSyncTTL {
		t.Fatal("sync TTL changed")
	}
}
