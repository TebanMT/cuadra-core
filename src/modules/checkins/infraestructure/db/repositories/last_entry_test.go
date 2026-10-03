package repositories

import (
	"github.com/google/uuid"
	"testing"
	"time"
)

func verifyLastEntry(t *testing.T, gym, member, otherGym, otherMember uuid.UUID,
	insert func(uuid.UUID, uuid.UUID, time.Time, string, bool),
	read func(uuid.UUID, uuid.UUID) (*time.Time, error)) {
	t.Helper()
	assertAt := func(g, m uuid.UUID, expected *time.Time) {
		t.Helper()
		actual, err := read(g, m)
		if err != nil {
			t.Fatal(err)
		}
		if expected == nil {
			if actual != nil {
				t.Fatalf("unexpected entry: %v", actual)
			}
			return
		}
		if actual == nil || !actual.Equal(*expected) {
			t.Fatalf("entry=%v, want=%v", actual, expected)
		}
	}
	assertAt(gym, member, nil)
	first := time.Now().UTC().Truncate(time.Millisecond).Add(-48 * time.Hour)
	insert(gym, member, first, "allowed_active", false)
	// Many newer denied attempts must not hide the real entry behind a page limit.
	for i := 0; i < 60; i++ {
		insert(gym, member, first.Add(time.Duration(i+1)*time.Minute), "denied_expired", false)
	}
	insert(gym, member, first.Add(2*time.Hour), "allowed_active", true)
	insert(otherGym, otherMember, first.Add(3*time.Hour), "allowed_active", false)
	assertAt(gym, member, &first)
	assertAt(otherGym, member, nil)
	assertAt(gym, otherMember, nil)
	override := first.Add(4 * time.Hour)
	insert(gym, member, override, "allowed_override", false)
	assertAt(gym, member, &override)
}
