package logocache

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Un logo enorme con fondo transparente sale reducido a 1024 px de lado
// mayor, proporcional, y CON transparencia (el dashboard lo pinta sobre
// fondos oscuros; el aplanado sobre blanco es sólo del ticket).
func TestPrepareUpload_ReduceYConservaTransparencia(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 3000, 1500))
	for y := 500; y < 1000; y++ {
		for x := 1000; x < 2000; x++ {
			src.Set(x, y, color.NRGBA{200, 30, 30, 255})
		}
	}
	out, err := PrepareUpload(encodePNG(t, src))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("no es PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 1024 || b.Dy() != 512 {
		t.Fatalf("tamaño = %v, want 1024x512", b)
	}
	if _, _, _, a := img.At(5, 5).RGBA(); a != 0 {
		t.Errorf("esquina transparente quedó con alpha %d", a)
	}
	if r, _, _, a := img.At(512, 256).RGBA(); a == 0 || r>>8 < 150 {
		t.Errorf("el centro del logo se perdió (r=%d a=%d)", r>>8, a)
	}
}

// Un JPEG chico (p.ej. foto del logo) se acepta, no se agranda y sale PNG.
func TestPrepareUpload_JPEGChicoSinAgrandar(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 300, 120))
	var b bytes.Buffer
	if err := jpeg.Encode(&b, src, nil); err != nil {
		t.Fatal(err)
	}
	out, err := PrepareUpload(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || format != "png" || cfg.Width != 300 || cfg.Height != 120 {
		t.Fatalf("cfg=%+v format=%q err=%v", cfg, format, err)
	}
}

func TestPrepareUpload_RechazaLoQueNoEsImagen(t *testing.T) {
	if _, err := PrepareUpload([]byte("MZ\x90\x00 esto es un exe")); !errors.Is(err, ErrUnsupportedImage) {
		t.Fatalf("err = %v, want ErrUnsupportedImage", err)
	}
}
