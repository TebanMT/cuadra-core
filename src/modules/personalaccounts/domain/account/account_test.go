package account

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/google/uuid"
)

var profileNow = time.Date(2026, 10, 19, 1, 2, 3, 456000000, time.UTC)

func initialAccount(t *testing.T) *Account {
	t.Helper()
	a, err := NewAccount(uuid.New(), profileNow.In(time.FixedZone("gym", -6*3600)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestInitialProfileAndUnicodeUpdate(t *testing.T) {
	a := initialAccount(t)
	if a.DisplayName != "" || a.WeightUnit != Kilograms || a.Status != Active || a.Revision != 1 || a.CreatedAt.Location() != time.UTC || a.UpdatedAt.Location() != time.UTC {
		t.Fatalf("unexpected initial profile: %#v", a)
	}
	name := strings.Repeat("💪", 80)
	if err := a.UpdateProfile(name, Pounds, 1, profileNow.Add(time.Minute).In(time.FixedZone("gym", -6*3600))); err != nil {
		t.Fatal("80 Unicode codepoints should fit", err)
	}
	if a.DisplayName != name || a.WeightUnit != Pounds || a.Revision != 2 || a.Status != Active || !a.CreatedAt.Equal(profileNow) || !a.UpdatedAt.Equal(profileNow.Add(time.Minute)) || a.UpdatedAt.Location() != time.UTC {
		t.Fatal("profile update changed unrelated state or time", a)
	}
	if err := a.UpdateProfile("  Ana María  ", Kilograms, 2, profileNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if a.DisplayName != "Ana María" || a.Revision != 3 {
		t.Fatal("profile was not normalized once")
	}
}

func TestProfileValidationNeverPartiallyMutates(t *testing.T) {
	for _, tc := range []struct {
		name, displayName string
		unit              WeightUnit
		expected          int64
		now               time.Time
		want              error
	}{
		{"empty", "", Kilograms, 1, profileNow, personalErrors.ErrInvalidDisplayName},
		{"whitespace", " \t\n", Kilograms, 1, profileNow, personalErrors.ErrInvalidDisplayName},
		{"too long", strings.Repeat("á", 81), Kilograms, 1, profileNow, personalErrors.ErrInvalidDisplayName},
		{"nul", "Ana\x00", Kilograms, 1, profileNow, personalErrors.ErrInvalidDisplayName},
		{"invalid UTF8", "Ana\xff", Kilograms, 1, profileNow, personalErrors.ErrInvalidDisplayName},
		{"unit", "Ana", "KG", 1, profileNow, personalErrors.ErrInvalidWeightUnit},
		{"zero revision", "Ana", Pounds, 0, profileNow, personalErrors.ErrInvalidProfileRevision},
		{"stale revision", "Ana", Pounds, 2, profileNow, personalErrors.ErrProfileRevisionConflict},
		{"missing time", "Ana", Pounds, 1, time.Time{}, personalErrors.ErrInvalidAccount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := initialAccount(t)
			before := *a
			if err := a.UpdateProfile(tc.displayName, tc.unit, tc.expected, tc.now); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if *a != before {
				t.Fatal("failed update mutated profile")
			}
		})
	}
}

func TestUnavailableAccountsAndOverflowCannotChangeProfile(t *testing.T) {
	for _, status := range []Status{DeletionPending, Deleted} {
		a := initialAccount(t)
		a.Status = status
		before := *a
		if err := a.UpdateProfile("Ana", Pounds, 1, profileNow); !errors.Is(err, personalErrors.ErrAccountUnavailable) {
			t.Fatal(err)
		}
		if *a != before {
			t.Fatal("unavailable account was changed")
		}
	}
	a := initialAccount(t)
	deleted := profileNow
	a.DeletedAt = &deleted
	if err := a.UpdateProfile("Ana", Pounds, 1, profileNow); !errors.Is(err, personalErrors.ErrAccountUnavailable) {
		t.Fatal("soft deletion ignored", err)
	}
	a = initialAccount(t)
	a.Revision = math.MaxInt64
	if err := a.UpdateProfile("Ana", Pounds, math.MaxInt64, profileNow); !errors.Is(err, personalErrors.ErrProfileRevisionExhausted) {
		t.Fatal("revision overflow accepted", err)
	}
	if a.Revision != math.MaxInt64 || a.DisplayName != "" {
		t.Fatal("overflow mutated account")
	}
}

func TestProfileWriteRequiresNextValidatedRevision(t *testing.T) {
	a := initialAccount(t)
	if err := a.ValidateProfileWrite(1); !errors.Is(err, personalErrors.ErrInvalidProfileRevision) {
		t.Fatal("unmodified aggregate accepted", err)
	}
	if err := a.UpdateProfile("Ana", Pounds, 1, profileNow); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateProfileWrite(1); err != nil {
		t.Fatal(err)
	}
	a.Revision = 4
	if err := a.ValidateProfileWrite(1); !errors.Is(err, personalErrors.ErrInvalidProfileRevision) {
		t.Fatal("skipped revision accepted", err)
	}
	a.Revision = 2
	a.DisplayName = ""
	if err := a.ValidateProfileWrite(1); !errors.Is(err, personalErrors.ErrInvalidDisplayName) {
		t.Fatal("empty write accepted", err)
	}
	if _, err := NewAccount(uuid.Nil, profileNow); !errors.Is(err, personalErrors.ErrInvalidAccount) {
		t.Fatal("nil identity accepted", err)
	}
}
