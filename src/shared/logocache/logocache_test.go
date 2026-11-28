package logocache

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func pngBytes(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestNormalize_ResizesAndFlattensTransparency(t *testing.T) {
	out, err := Normalize(pngBytes(t, 1024, 256, color.NRGBA{0, 0, 0, 0}))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != maxWidthPx || img.Bounds().Dy() != 128 {
		t.Fatalf("size = %v", img.Bounds())
	}
	if r, g, b, _ := img.At(10, 10).RGBA(); r != 0xffff || g != 0xffff || b != 0xffff {
		t.Fatalf("transparente debe quedar blanco (papel), got %v %v %v", r, g, b)
	}
	if _, err := Normalize([]byte("<svg/>")); err == nil {
		t.Fatal("formato no soportado debe fallar")
	}
}

func TestPublicIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.5", "172.16.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fd00::1"} {
		if publicIP(net.ParseIP(s)) {
			t.Errorf("%s no debería ser público", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "104.16.0.1", "2606:4700::1"} {
		if !publicIP(net.ParseIP(s)) {
			t.Errorf("%s debería ser público", s)
		}
	}
}

func TestPNG_RefusesInternalHosts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(pngBytes(t, 10, 10, color.Black))
	}))
	defer srv.Close()
	c := New("")
	if _, err := c.PNG(context.Background(), "gym-1", srv.URL+"/logo.png"); err == nil {
		t.Fatal("un logo en 127.0.0.1 debe rechazarse (SSRF)")
	}
}

func TestPNG_ServesDiskCopyOffline(t *testing.T) {
	dir := t.TempDir()
	url := "https://logo.invalid/logo.png" // nunca resuelve
	key := cacheKey("gym-1", url)
	c := New(dir)
	want := pngBytes(t, 4, 4, color.Black)
	c.writeDisk("gym-1", key, want)

	fresh := New(dir) // reinicio del sidecar: memoria vacía
	got, err := fresh.PNG(context.Background(), "gym-1", url)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("debe servir la copia en disco sin internet: err=%v", err)
	}
	if _, err := fresh.PNG(context.Background(), "gym-1", ""); err == nil {
		t.Fatal("sin url = sin logo")
	}
}
