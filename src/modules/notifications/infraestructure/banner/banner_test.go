package banner

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"testing"
)

// TestCopyFor_MemberSaysNumeroDeSocio ancla el cambio de copy de ADR-010: el
// banner del SOCIO muestra "NÚMERO DE SOCIO" (antes "PIN DE ACCESO"), mientras
// que el del OPERADOR sigue siendo su PIN de login (concepto aparte, users BC).
func TestCopyFor_MemberSaysNumeroDeSocio(t *testing.T) {
	label, fieldLbl, _, _ := copyFor(Input{Kind: KindMember, Name: "Esteban", GymName: "Gym Bros", PIN: "4827"})
	if label != "NÚMERO DE SOCIO" {
		t.Errorf("member banner label = %q, want %q", label, "NÚMERO DE SOCIO")
	}
	if fieldLbl != "SOCIO" {
		t.Errorf("member field label = %q, want %q", fieldLbl, "SOCIO")
	}
	opLabel, _, _, _ := copyFor(Input{Kind: KindOperator, Name: "Laura", GymName: "Gym Bros", PIN: "1593"})
	if opLabel != "PIN DE ACCESO" {
		t.Errorf("operator banner label = %q, want %q (no debe cambiar)", opLabel, "PIN DE ACCESO")
	}
}

// TestRender_MemberBannerArtifact renderiza un banner de muestra del socio y,
// si TINTA_BANNER_ARTIFACT está seteada, lo escribe a esa ruta para inspección
// visual (verificación manual de ADR-010). Siempre valida que renderice un PNG.
func TestRender_MemberBannerArtifact(t *testing.T) {
	body, ct, err := Render(Input{Kind: KindMember, Name: "Esteban Cuadra", GymName: "Iron House Gym", PIN: "4827"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if ct != "image/png" {
		t.Fatalf("content type = %q", ct)
	}
	if path := os.Getenv("TINTA_BANNER_ARTIFACT"); path != "" {
		if werr := os.WriteFile(path, body, 0o644); werr != nil {
			t.Fatalf("write artifact: %v", werr)
		}
		t.Logf("banner de muestra escrito en %s (%d bytes)", path, len(body))
	}
}

func TestRender_ProducesValidPNG(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Input
	}{
		{"member", Input{Kind: KindMember, Name: "Esteban", GymName: "Gym Bros", PIN: "4827"}},
		{"operator", Input{Kind: KindOperator, Name: "Laura", GymName: "Gym Bros", PIN: "1593"}},
		{"owner", Input{Kind: KindOwner, Name: "Esteban", GymName: "Iron House", PIN: "4827"}},
		{"member sin nombre", Input{Kind: KindMember, GymName: "Gym Bros", PIN: "0001"}},
		{"acentos", Input{Kind: KindMember, Name: "Ángel", GymName: "Gimnasio Ñú", PIN: "9999"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, ct, err := Render(tc.in)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if ct != "image/png" {
				t.Fatalf("content type = %q", ct)
			}
			cfg, err := png.DecodeConfig(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("not a valid PNG: %v", err)
			}
			if cfg.Width != canvasW || cfg.Height != canvasH {
				t.Errorf("dims = %dx%d, want %dx%d", cfg.Width, cfg.Height, canvasW, canvasH)
			}
			if _, _, err := image.Decode(bytes.NewReader(body)); err != nil {
				t.Fatalf("full decode failed: %v", err)
			}
		})
	}
}
