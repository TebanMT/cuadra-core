//go:build server

package imagegen

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"

	"github.com/google/uuid"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/cuadra/cuadra-core/src/shared/r2"
)

const (
	bannerW = 800
	bannerH = 418
)

// WelcomeBannerGen generates a PNG welcome banner with the PIN embedded
// and uploads it to R2. Implements notifications/app.WelcomeImageGenerator.
type WelcomeBannerGen struct {
	r2 *r2.Client
}

// NewWelcomeBannerGen creates a generator that uploads banners to the
// given R2 client (should be the public bucket client).
func NewWelcomeBannerGen(r2Client *r2.Client) *WelcomeBannerGen {
	return &WelcomeBannerGen{r2: r2Client}
}

// Generate draws a banner with the PIN, uploads it to R2, and returns the
// public URL. The R2 key uses a UUID so the URL is opaque (does not expose
// the PIN).
func (g *WelcomeBannerGen) Generate(ctx context.Context, pin string) (string, error) {
	img := drawBanner(pin)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}

	key := "welcome-banners/" + uuid.New().String() + ".png"
	if err := g.r2.PutObject(ctx, key, "image/png", &buf, int64(buf.Len())); err != nil {
		return "", err
	}
	return g.r2.PublicURL(key), nil
}

func drawBanner(pin string) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, bannerW, bannerH))

	// Background: #111827
	bg := color.RGBA{R: 0x11, G: 0x18, B: 0x27, A: 0xff}
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)

	orange := color.RGBA{R: 0xf9, G: 0x73, B: 0x16, A: 0xff}
	orangeSubtle := color.RGBA{R: 0xf9, G: 0x73, B: 0x16, A: 0x66} // alpha ~0.4

	// Orange bars top and bottom (5px)
	drawRect(img, 0, 0, bannerW, 5, orange)
	drawRect(img, 0, bannerH-5, bannerW, bannerH, orange)

	// Vertical decorative bars — left and right (subtle)
	drawRect(img, 0, 30, 5, 100, orangeSubtle)
	drawRect(img, bannerW-5, 30, bannerW, 100, orangeSubtle)
	drawRect(img, 0, bannerH-100, 5, bannerH-30, orangeSubtle)
	drawRect(img, bannerW-5, bannerH-100, bannerW, bannerH-30, orangeSubtle)

	// Decorative circles (outline, very subtle)
	circleColor := color.RGBA{R: 0xf9, G: 0x73, B: 0x16, A: 0x22}
	drawCircleOutline(img, bannerW-80, 80, 60, 2, circleColor)
	drawCircleOutline(img, 80, bannerH-80, 60, 2, circleColor)
	drawCircleOutline(img, bannerW-80, 80, 90, 1, circleColor)
	drawCircleOutline(img, 80, bannerH-80, 90, 1, circleColor)

	// Parse font
	ft, err := opentype.Parse(gobold.TTF)
	if err != nil {
		// Fallback: just return the background with rectangles
		return img
	}

	// Label: "TU CÓDIGO PERSONAL" — ~20px, gray #9ca3af
	labelFace, err := opentype.NewFace(ft, &opentype.FaceOptions{
		Size:    20,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err == nil {
		defer labelFace.Close()
		gray := color.RGBA{R: 0x9c, G: 0xa3, B: 0xaf, A: 0xff}
		d := &font.Drawer{Dst: img, Src: &image.Uniform{gray}, Face: labelFace}
		centerText(d, "TU CÓDIGO PERSONAL", 200)
	}

	// PIN — ~110px, white, centered ~220px from top
	pinFace, err := opentype.NewFace(ft, &opentype.FaceOptions{
		Size:    110,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err == nil {
		defer pinFace.Close()
		white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
		d := &font.Drawer{Dst: img, Src: &image.Uniform{white}, Face: pinFace}
		centerText(d, pin, 320)
	}

	// Decorative line below the PIN: 200×4px, orange, centered
	lineX := (bannerW - 200) / 2
	drawRect(img, lineX, 340, lineX+200, 344, orange)

	return img
}

func centerText(d *font.Drawer, text string, y int) {
	advance := d.MeasureString(text)
	d.Dot = fixed.Point26_6{
		X: fixed.I(bannerW/2) - advance/2,
		Y: fixed.I(y),
	}
	d.DrawString(text)
}

func drawRect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func drawCircleOutline(img *image.RGBA, cx, cy, radius, thickness int, c color.RGBA) {
	for r := radius; r <= radius+thickness; r++ {
		for angle := 0.0; angle < 2*math.Pi; angle += 0.005 {
			x := cx + int(float64(r)*math.Cos(angle))
			y := cy + int(float64(r)*math.Sin(angle))
			if x >= 0 && x < img.Bounds().Max.X && y >= 0 && y < img.Bounds().Max.Y {
				img.SetRGBA(x, y, c)
			}
		}
	}
}
