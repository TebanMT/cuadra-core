package reports

import "testing"

// TestAsciiSafe_EncodesLatinAccents locks in the fix for "PDF acentos se
// veían rotos": asciiSafe used to leak UTF-8 bytes into gofpdf (cp1252),
// so "á" came out as "Ã¡". The fix is a UTF-8 → cp1252 byte translation
// (charmap.Windows1252.EncodeRune). Each rune must encode to exactly one
// byte for cp1252-friendly glyphs.
func TestAsciiSafe_EncodesLatinAccents(t *testing.T) {
	cases := map[string][]byte{
		"a":          {0x61},                                                       // ASCII passthrough
		"á":          {0xE1},                                                       // Latin-1
		"é":          {0xE9},
		"í":          {0xED},
		"ó":          {0xF3},
		"ú":          {0xFA},
		"ñ":          {0xF1},
		"Á":          {0xC1},
		"¿":          {0xBF},
		"¡":          {0xA1},
		"—":          {0x97}, // em-dash: U+2014 → cp1252 byte 0x97
		"Categoría":  {0x43, 0x61, 0x74, 0x65, 0x67, 0x6F, 0x72, 0xED, 0x61},
		"Atención":   {0x41, 0x74, 0x65, 0x6E, 0x63, 0x69, 0xF3, 0x6E},
		"períodos":   {0x70, 0x65, 0x72, 0xED, 0x6F, 0x64, 0x6F, 0x73},
		"日本":         {0x3F, 0x3F}, // CJK falls back to '?'
	}
	for in, want := range cases {
		got := []byte(asciiSafe(in))
		if string(got) != string(want) {
			t.Errorf("asciiSafe(%q) = % X, want % X", in, got, want)
		}
	}
}
