//go:build server && (darwin || linux)

package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalHelperKeysAreIndependentAndNeverOverwrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys.json")
	if err := run("keys", p); err != nil {
		t.Fatal(err)
	}
	raw, err := privateFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil {
		t.Fatal("keys document invalid")
	}
	var seed, public string
	_ = json.Unmarshal(keys["jwt_seed"], &seed)
	var publics map[string]string
	_ = json.Unmarshal(keys["jwt_verification_keys"], &publics)
	public = publics["local-v1"]
	s, _ := base64.RawURLEncoding.DecodeString(seed)
	pb, _ := base64.RawURLEncoding.DecodeString(public)
	if len(s) != 32 || !bytes.Equal(ed25519.NewKeyFromSeed(s).Public().(ed25519.PublicKey), pb) {
		t.Fatal("JWT public binding invalid")
	}
	seen := map[string]bool{seed: true}
	for _, name := range []string{"envelope_keys", "recovery_keys", "cursor_keys", "phone_lock_key", "budget_hash_key"} {
		var value string
		if strings.HasSuffix(name, "_keys") {
			var ring struct {
				Keys map[string]string `json:"keys"`
			}
			_ = json.Unmarshal(keys[name], &ring)
			value = ring.Keys["local-v1"]
		} else {
			_ = json.Unmarshal(keys[name], &value)
		}
		b, e := base64.RawURLEncoding.Strict().DecodeString(value)
		if e != nil || len(b) != 32 || seen[value] {
			t.Fatal("purpose key reused or invalid")
		}
		seen[value] = true
	}
	if run("keys", p) == nil {
		t.Fatal("existing keys overwritten")
	}
	other, _ := os.ReadFile(p)
	if !bytes.Equal(raw, other) {
		t.Fatal("keys changed on retry")
	}
}

func TestLocalHelperRejectsPublicLinksAndOversizedInputs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "private.json")
	if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := privateFile(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Link(p, filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	if _, err := privateFile(p); err == nil {
		t.Fatal("hardlink accepted")
	}
	_ = os.Remove(filepath.Join(dir, "hard"))
	_ = os.Chmod(p, 0644)
	if _, err := privateFile(p); err == nil {
		t.Fatal("public input accepted")
	}
	_ = os.Chmod(p, 0600)
	_ = os.WriteFile(p, bytes.Repeat([]byte("a"), 65537), 0600)
	if _, err := privateFile(p); err == nil {
		t.Fatal("large input accepted")
	}
}

func TestLocalHelperLedgerInitializationIsExplicitAndStatusDoesNotSpend(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.json")
	ledger := filepath.Join(dir, "ledger.json")
	c := map[string]any{"verify": map[string]any{"account_sid": "AC" + strings.Repeat("0", 32), "service_sid": "VA" + strings.Repeat("0", 32), "username": "AC" + strings.Repeat("0", 32), "password": "synthetic-auth-token", "validity_seconds": 600, "timeout_seconds": 20}, "local_verify_guard": map[string]any{"ledger_path": ledger, "destinations": []string{"+525555559901", "+525555559902"}}}
	raw, _ := json.Marshal(c)
	if err = os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if run("ledger-status", p) == nil {
		t.Fatal("status initialized absent ledger")
	}
	if _, err = os.Stat(ledger); !os.IsNotExist(err) {
		t.Fatal("status created ledger")
	}
	if err = run("ledger-init", p); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(ledger)
	if run("ledger-init", p) == nil {
		t.Fatal("ledger reinitialized")
	}
	if err = run("ledger-status", p); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(ledger)
	if !bytes.Equal(before, after) || !bytes.Contains(after, []byte(`"starts":[]`)) {
		t.Fatal("status changed reservations")
	}
}
