package device

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDevicesRevocationTargetIsExclusiveAndClosed(t *testing.T) {
	for _, target := range []RevocationTarget{{DeviceID: uuid.New()}, {Scope: RevokeOthers}, {Scope: RevokeAll}} {
		if target.Validate() != nil {
			t.Fatal("valid revocation selector rejected")
		}
	}
	for _, target := range []RevocationTarget{{}, {Scope: "current"}, {Scope: "ALL"}, {DeviceID: uuid.New(), Scope: RevokeOthers}, {DeviceID: uuid.UUID{1}}} {
		if target.Validate() == nil {
			t.Fatal("ambiguous or invalid revocation selector accepted")
		}
	}
}

func TestDevicesListedProjectionRejectsInvalidChronologyAndIdentity(t *testing.T) {
	at := time.Date(2026, 10, 19, 12, 0, 0, 123456000, time.UTC)
	d := ListedDevice{ID: uuid.New(), AccountID: uuid.New(), Platform: "ios", Label: "Mi teléfono", AuthorizedAt: at}
	if d.Validate() != nil || (ListPosition{ID: d.ID, AuthorizedAt: at}).Validate() != nil {
		t.Fatal("valid projection or microsecond position rejected")
	}
	before := at.Add(-time.Microsecond)
	for _, change := range []func(*ListedDevice){
		func(d *ListedDevice) { d.ID = uuid.Nil }, func(d *ListedDevice) { d.AccountID = uuid.Nil },
		func(d *ListedDevice) { d.Platform = "web" }, func(d *ListedDevice) { d.Label = "" },
		func(d *ListedDevice) { d.Label = strings.Repeat("a", 81) }, func(d *ListedDevice) { d.Label = "\xff" },
		func(d *ListedDevice) { d.AuthorizedAt = time.Time{} }, func(d *ListedDevice) { d.RevokedAt = &before },
		func(d *ListedDevice) { d.LastSeenAt = &before },
	} {
		changed := d
		change(&changed)
		if changed.Validate() == nil {
			t.Fatal("invalid device projection accepted")
		}
	}
	for _, p := range []ListPosition{{ID: d.ID}, {AuthorizedAt: at}, {ID: d.ID, AuthorizedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}} {
		if p.Validate() == nil {
			t.Fatal("invalid keyset accepted")
		}
	}
}
