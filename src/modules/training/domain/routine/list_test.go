package routine

import (
	"strings"
	"testing"
)

func TestRoutineSearchNormalizesCaseAccentsAndUnicodeWhitespace(t *testing.T) {
	for raw, want := range map[string]string{
		"  PIÉRNA\t Completa\u00a0": "pierna completa", "Pie\u0301rna": "pierna", "\u2003ÍSTANBUL\n漢 한": "istanbul 漢 한", "100%_\\ literal": "100%_\\ literal", "\t\n": "",
		"Straße STRASSE STRAẞE": "strasse strasse strasse", "ΟΣ ος οσ Σ ς σ": "οσ οσ οσ σ σ σ", "ﬀ ﬁ ﬂ ﬃ ﬄ ﬅ ﬆ": "ff fi fl ffi ffl st st",
		"ſ É\u0301 ǰ": "s e j", "և ԵՒ": "եւ եւ", "Ꭰ ꭰ": "ꭰ ꭰ", "Ａ ａ": "ａ ａ",
	} {
		if got := NormalizeSearch(raw); got != want || NormalizeSearch(got) != got {
			t.Errorf("normalize %q: %q", raw, got)
		}
	}
	for _, in := range []string{strings.Repeat("a", 121), "bad\x00", string([]byte{0xff})} {
		if _, err := NormalizeListFilter("active", in); err == nil {
			t.Fatal("invalid search accepted")
		}
	}
	if _, err := NormalizeListFilter("ready", ""); err == nil {
		t.Fatal("unsupported list status accepted")
	}
	if filter, err := NormalizeListFilter("", ""); err != nil || filter.Status != "active" {
		t.Fatal("default status", err)
	}
	if _, err := NormalizeListFilter("all", strings.Repeat("한", 120)); err != nil {
		t.Fatal("valid 120-scalar search rejected", err)
	}
}

func TestRoutineSearchUnicodeExpansionKeepsOriginalInputLimit(t *testing.T) {
	for _, vector := range []struct{ raw, want string }{
		{strings.Repeat("ß", 120), strings.Repeat("ss", 120)}, {strings.Repeat("ﬃ", 120), strings.Repeat("ffi", 120)},
		{strings.Repeat("한", 120), strings.Repeat("한", 120)},
	} {
		filter, err := NormalizeListFilter("active", vector.raw)
		if err != nil || filter.Query != vector.want || NormalizeSearch(filter.Query) != filter.Query {
			t.Fatal("valid bounded input changed or became noncanonical", err)
		}
		if _, err := NormalizeListFilter("active", vector.raw+"x"); err == nil {
			t.Fatal("expansion bypassed original 120-scalar bound")
		}
	}
}
