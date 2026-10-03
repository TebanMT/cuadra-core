package repositories

import (
	repo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	"github.com/google/uuid"
	"testing"
	"time"
)

func verifyHistoryPaging(t *testing.T, gym uuid.UUID, insert func(uuid.UUID, time.Time, string, bool), list func(int, int) ([]repo.RecentCheckinRow, error)) {
	t.Helper()
	// All entries share a timestamp: id breaks ties, so changing pages is stable.
	at := time.Date(2026, 9, 29, 1, 0, 0, 0, time.UTC) // Sep 28 in Mexico City.
	for i := 0; i < 205; i++ {
		insert(gym, at, "allowed_active", false)
	}
	insert(gym, at, "denied_expired", false)
	insert(gym, at, "allowed_active", true)
	insert(uuid.New(), at, "allowed_active", false)
	insert(gym, at.Add(5*time.Hour), "allowed_active", false) // next local day
	seen := map[uuid.UUID]bool{}
	for page := 0; page < 5; page++ {
		rows, err := list(51, page*50)
		if err != nil {
			t.Fatal(err)
		}
		want := 51
		if page == 4 {
			want = 5
		}
		if len(rows) != want {
			t.Fatalf("page %d: got %d rows, want %d", page+1, len(rows), want)
		}
		if len(rows) > 50 {
			rows = rows[:50]
		}
		for _, row := range rows {
			if seen[row.ID] {
				t.Fatalf("duplicate entry across pages: %s", row.ID)
			}
			seen[row.ID] = true
		}
	}
	if len(seen) != 205 {
		t.Fatalf("got %d distinct entries", len(seen))
	}
	rows, err := list(50, 250)
	if err != nil || len(rows) != 0 {
		t.Fatalf("end of history: %v, %d rows", err, len(rows))
	}
}
