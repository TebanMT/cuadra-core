package sidecartoken

import (
	"bytes"
	"strings"
	"testing"
)

func TestGenerate_DistinctEachCall(t *testing.T) {
	tok1, h1, err := Generate()
	if err != nil {
		t.Fatalf("gen1: %v", err)
	}
	tok2, h2, err := Generate()
	if err != nil {
		t.Fatalf("gen2: %v", err)
	}
	if tok1 == tok2 {
		t.Errorf("two calls returned identical tokens")
	}
	if bytes.Equal(h1, h2) {
		t.Errorf("two calls returned identical hashes")
	}
	if !strings.HasPrefix(tok1, Prefix) || !strings.HasPrefix(tok2, Prefix) {
		t.Errorf("tokens missing prefix")
	}
	if len(h1) != 32 {
		t.Errorf("hash length = %d, want 32", len(h1))
	}
}

func TestHash_DeterministicAndMatchesGenerate(t *testing.T) {
	tok, h, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	again := Hash(tok)
	if !bytes.Equal(h, again) {
		t.Errorf("Hash(%q) does not match Generate's hash", tok)
	}
}

func TestHasPrefix(t *testing.T) {
	if !HasPrefix("sk_live_abc") {
		t.Error("HasPrefix should match sk_live_*")
	}
	if HasPrefix("eyJhbGc...") {
		t.Error("HasPrefix should reject JWT-looking tokens")
	}
	if HasPrefix("") {
		t.Error("HasPrefix should reject empty")
	}
}
