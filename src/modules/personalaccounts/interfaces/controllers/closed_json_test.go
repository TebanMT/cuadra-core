//go:build server

package controllers

import (
	"encoding/json"
	"testing"
)

func TestClosedJSONRejectsCaseAliasesAtEveryObjectBoundary(t *testing.T) {
	var body struct {
		Code      string `json:"code"`
		Candidate struct {
			Label string `json:"label"`
		} `json:"candidate"`
		Signature json.RawMessage `json:"device_signature"`
	}
	for _, raw := range []string{`{"Code":"123456"}`, `{"code":"123456","Code":"999999"}`, `{"candidate":{"Label":"other"}}`, `{"candidate":{"label":"ok","LABEL":"other"}}`, `null`, `[]`} {
		if decodeClosedJSON([]byte(raw), &body) == nil {
			t.Fatal("closed contract accepted an alias or non-object")
		}
	}
	if err := decodeClosedJSON([]byte(`{"code":"123456","candidate":{"label":"exact"}}`), &body); err != nil || body.Code != "123456" || body.Candidate.Label != "exact" {
		t.Fatal("canonical fields rejected")
	}
}
