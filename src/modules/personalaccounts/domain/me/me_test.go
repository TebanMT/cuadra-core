package me

import (
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	"github.com/google/uuid"
)

func TestCapabilitiesRequireExplicitConfigurationAndClone(t *testing.T) {
	c := Capabilities{ProtocolVersion: 1, MinimumProtocolVersion: 1, MinimumAppVersion: "0.0.0", MaxPushOperations: 50, MaxRequestBytes: 2097152, MaxGroupBytes: 1048576, Features: []string{}}
	if c.Validate() != nil {
		t.Fatal("explicit inactive capabilities rejected")
	}
	for _, mutate := range []func(*Capabilities){func(c *Capabilities) { c.Features = nil }, func(c *Capabilities) { c.MinimumAppVersion = "" }, func(c *Capabilities) { c.ProtocolVersion = 0 }, func(c *Capabilities) { c.MaxPushOperations = 0 }, func(c *Capabilities) { c.Features = []string{"personal_sync", "personal_sync"} }, func(c *Capabilities) { c.Features = []string{"unimplemented"} }} {
		bad := c.Clone()
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid policy silently defaulted")
		}
	}
	version := "synthetic-policy"
	c.PolicyVersion = &version
	c.Features = []string{"workouts"}
	copy := c.Clone()
	c.Features[0] = "personal_sync"
	version = "mutated"
	if copy.Features[0] != "workouts" || *copy.PolicyVersion != "synthetic-policy" {
		t.Fatal("capability configuration alias")
	}
}

func TestMeSnapshotRejectsUnmaskedPhone(t *testing.T) {
	a, err := account.NewAccount(uuid.New(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s := Snapshot{Profile: *a, SecurityVersion: 1, PhoneMasked: "•••• 5555"}
	if s.Validate() != nil {
		t.Fatal("valid snapshot rejected")
	}
	for _, phone := range []string{"+525555555555", "5555", "•••• 555X", "•••• 55555", ""} {
		s.PhoneMasked = phone
		if s.Validate() == nil {
			t.Fatal("unexpected phone projection accepted")
		}
	}
}
