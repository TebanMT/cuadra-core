package timezone

import "testing"

func TestIANACatalogueRejectsHostLocalAndFallbacks(t *testing.T) {
	zones := NewIANA()
	for _, name := range []string{"UTC", "Etc/UTC", "America/Mexico_City", "America/New_York", "Asia/Kathmandu", "Pacific/Chatham"} {
		if !zones.Valid(name) {
			t.Fatalf("embedded zone unavailable: %s", name)
		}
	}
	for _, name := range []string{"", "Local", "localtime", "posixrules", "posix/America/New_York", "right/UTC", "../UTC", "/etc/localtime", " UTC", "UTC ", "+06:00", "America/Invented", "UTC\x00"} {
		if zones.Valid(name) {
			t.Fatalf("non-IANA zone accepted: %q", name)
		}
	}
}
