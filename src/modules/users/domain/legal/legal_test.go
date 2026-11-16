package legal

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCurrentAcceptancesCoversBothDocuments(t *testing.T) {
	gym, user := uuid.New(), uuid.New()
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.FixedZone("CDMX", -6*3600))
	got := CurrentAcceptances(gym, user, at, "201.141.10.4", "Mozilla/5.0")
	if len(got) != 2 {
		t.Fatalf("want 2 acceptances, got %d", len(got))
	}
	want := map[string]string{DocumentTerms: TermsVersion, DocumentPrivacy: PrivacyVersion}
	for _, a := range got {
		if want[a.Document] != a.Version {
			t.Errorf("%s: version %q, want %q", a.Document, a.Version, want[a.Document])
		}
		if a.GymID != gym || a.UserID != user || a.ID == uuid.Nil {
			t.Errorf("%s: ids not set: %+v", a.Document, a)
		}
		if a.AcceptedAt.Location() != time.UTC || !a.AcceptedAt.Equal(at) {
			t.Errorf("%s: accepted_at %v, want %v in UTC", a.Document, a.AcceptedAt, at)
		}
	}
	if got[0].ID == got[1].ID {
		t.Error("both acceptances share an id")
	}
}

func TestCurrentAcceptancesClipsLongRequestMetadata(t *testing.T) {
	ua := strings.Repeat("ñ", maxUserAgentLen+50)
	ip := strings.Repeat("1", maxIPLen+10)
	a := CurrentAcceptances(uuid.New(), uuid.New(), time.Now(), ip, ua)[0]
	if n := len([]rune(a.UserAgent)); n != maxUserAgentLen {
		t.Errorf("user agent kept %d runes, want %d", n, maxUserAgentLen)
	}
	if n := len([]rune(a.IPAddress)); n != maxIPLen {
		t.Errorf("ip kept %d runes, want %d", n, maxIPLen)
	}
}
