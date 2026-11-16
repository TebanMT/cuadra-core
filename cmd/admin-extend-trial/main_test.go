//go:build server

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidateDays(t *testing.T) {
	for _, days := range []int{0, -3, maxExtensionDays + 1} {
		if validateDays(days) == nil {
			t.Errorf("validateDays(%d) = nil, want error", days)
		}
	}
	for _, days := range []int{1, 14, maxExtensionDays} {
		if err := validateDays(days); err != nil {
			t.Errorf("validateDays(%d) = %v, want nil", days, err)
		}
	}
}

// Mismo gym, mismos días, mismo día UTC: una sola extensión.
func TestTrialExtendedExternalIDIsPerGymDaysAndDay(t *testing.T) {
	gym := uuid.New()
	morning := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	evening := time.Date(2026, 11, 13, 22, 0, 0, 0, time.UTC)
	if trialExtendedExternalID(gym, 14, morning) != trialExtendedExternalID(gym, 14, evening) {
		t.Fatal("same gym, days and day must share the key")
	}
	if trialExtendedExternalID(gym, 14, morning) == trialExtendedExternalID(gym, 7, morning) {
		t.Fatal("different days must not share the key")
	}
	if trialExtendedExternalID(gym, 14, morning) == trialExtendedExternalID(gym, 14, morning.Add(24*time.Hour)) {
		t.Fatal("another day must not share the key")
	}
	if !strings.HasPrefix(trialExtendedExternalID(gym, 14, morning), "trial-extended:"+gym.String()) {
		t.Fatal("key must name the gym")
	}
}
