// Package banner renders the WhatsApp welcome banners (UC member/operator
// onboarding) as PNG bytes. Per ADR-009 the welcome templates are Twilio
// *Media* templates: the member's access PIN can't ride as text (Meta won't
// approve PIN-in-text), so it's baked into this image and the image URL is
// passed as the template's media variable.
//
// Estética: "credencial digital / wallet pass" con la paleta de marca Tinta
// (papel + tinta navy + terracota). Hay dos temas — claro y oscuro — y cada
// banner elige uno de forma determinista a partir de su propia data (hash de
// gym+nombre+PIN). Así varía entre socios (se siente distintivo) pero es
// estable si el dispatcher reintenta el mismo mensaje.
//
// Tipografía: fuentes Go embebidas (golang.org/x/image/font/gofont) — sin
// assets externos. Los gráficos (huella, bordes, monograma) se dibujan con
// anti-aliasing manual para que se vean nítidos.
package banner

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Kind selects which onboarding banner to render.
type Kind int

const (
	KindMember Kind = iota
	KindOperator
	// KindOwner — banner de "tu sistema ya está vivo" que se manda al dueño
	// al vincularse el primer dispositivo del gym (ADR-010 / onboarding). El
	// PIN horneado es su código de acceso para entrar rápido a recepción.
	KindOwner
)

// Input is the data baked into the banner.
type Input struct {
	Kind    Kind
	Name    string
	GymName string
	PIN     string
}

const (
	canvasW = 1080
	canvasH = 1080
)

// theme — paleta completa de una variante (clara u oscura).
type theme struct {
	bg, card, header, onHeader     color.RGBA
	label, pin, fieldLbl, fieldVal color.RGBA
	footer, divider                color.RGBA
}

var lightTheme = theme{
	bg:       color.RGBA{0xFA, 0xF7, 0xF2, 0xff},
	card:     color.RGBA{0xFF, 0xFF, 0xFF, 0xff},
	header:   color.RGBA{0x0F, 0x1A, 0x2E, 0xff},
	onHeader: color.RGBA{0xFA, 0xF7, 0xF2, 0xff},
	label:    color.RGBA{0xD6, 0x59, 0x3C, 0xff},
	pin:      color.RGBA{0x0F, 0x1A, 0x2E, 0xff},
	fieldLbl: color.RGBA{0x6B, 0x72, 0x80, 0xff},
	fieldVal: color.RGBA{0x0F, 0x1A, 0x2E, 0xff},
	footer:   color.RGBA{0x6B, 0x72, 0x80, 0xff},
	divider:  color.RGBA{0xE5, 0xDF, 0xD2, 0xff},
}

var darkTheme = theme{
	bg:       color.RGBA{0x0F, 0x1A, 0x2E, 0xff},
	card:     color.RGBA{0x16, 0x22, 0x3A, 0xff},
	header:   color.RGBA{0xD6, 0x59, 0x3C, 0xff},
	onHeader: color.RGBA{0xFA, 0xF7, 0xF2, 0xff},
	label:    color.RGBA{0xF4, 0xA6, 0x88, 0xff},
	pin:      color.RGBA{0xFA, 0xF7, 0xF2, 0xff},
	fieldLbl: color.RGBA{0x9C, 0xA3, 0xAF, 0xff},
	fieldVal: color.RGBA{0xFA, 0xF7, 0xF2, 0xff},
	footer:   color.RGBA{0x9C, 0xA3, 0xAF, 0xff},
	divider:  color.RGBA{0x2A, 0x35, 0x50, 0xff},
}

// Render composes the banner and returns PNG bytes + the content type.
func Render(in Input) ([]byte, string, error) {
	reg, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return nil, "", fmt.Errorf("banner: parse regular font: %w", err)
	}
	bold, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return nil, "", fmt.Errorf("banner: parse bold font: %w", err)
	}

	th := lightTheme
	if pickDark(in) {
		th = darkTheme
	}

	dst := image.NewRGBA(image.Rect(0, 0, canvasW, canvasH))
	fillRect(dst, dst.Bounds(), th.bg)

	if in.Kind == KindOwner {
		renderOwner(dst, reg, bold, th, in)
	} else {
		renderCredential(dst, reg, bold, th, in)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, "", fmt.Errorf("banner: encode png: %w", err)
	}
	return buf.Bytes(), "image/png", nil
}

// renderCredential dibuja la credencial PIN de socio/operador (header con
// monograma + huella, PIN grande, nombre, footer + barcode).
func renderCredential(dst *image.RGBA, reg, bold *opentype.Font, th theme, in Input) {
	gym := orFallback(in.GymName, "Tu gym")
	labelTxt, fieldLbl, footer1, footer2 := copyFor(in)
	name := orFallback(in.Name, fieldLbl)

	// Tarjeta + header (esquinas superiores redondeadas).
	fillRoundRect(dst, 80, 80, 1000, 1000, 48, th.card)
	fillRoundRect(dst, 80, 80, 1000, 300, 48, th.header)
	fillRect(dst, image.Rect(80, 255, 1000, 300), th.header)

	// Monograma del gym + nombre + ícono de acceso (huella).
	fillDiscAA(dst, 185, 190, 58, th.onHeader)
	drawCenterX(dst, bold, 48, th.header, initials(gym), 185, 208)
	drawAtFit(dst, bold, 46, th.onHeader, gym, 280, 207, 560)
	drawFingerprint(dst, 905, 190, 78, 100, th.onHeader)

	// Cuerpo.
	drawAt(dst, bold, 27, th.label, labelTxt, 130, 435)
	drawAt(dst, bold, 132, th.pin, spacePIN(in.PIN), 124, 590)
	fillRect(dst, image.Rect(130, 650, 950, 654), th.divider)
	drawAt(dst, bold, 24, th.fieldLbl, fieldLbl, 130, 720)
	drawAtFit(dst, bold, 44, th.fieldVal, name, 130, 778, 820)

	// Footer + código de barras decorativo (carnet "escaneable").
	if footer2 == "" {
		drawCentered(dst, reg, 28, th.footer, footer1, 880)
	} else {
		drawCentered(dst, reg, 28, th.footer, footer1, 860)
		drawCentered(dst, reg, 28, th.footer, footer2, 900)
	}
	barcode(dst, 130, 950, 936, 34, th.pin)
}

// renderOwner dibuja el banner "¡Tu sistema ya está vivo!" que recibe el
// dueño al vincular el primer dispositivo: su código de acceso (PIN) como
// héroe + dos primeros pasos dentro del sistema.
func renderOwner(dst *image.RGBA, reg, bold *opentype.Font, th theme, in Input) {
	fillRoundRect(dst, 80, 80, 1000, 1000, 48, th.card)
	fillRoundRect(dst, 80, 80, 1000, 300, 48, th.header)
	fillRect(dst, image.Rect(80, 255, 1000, 300), th.header)

	// Header: monograma Tinta + título.
	fillDiscAA(dst, 180, 190, 54, th.onHeader)
	drawCenterX(dst, bold, 52, th.header, "T", 180, 210)
	drawAtFit(dst, bold, 46, th.onHeader, "¡Tu sistema ya está vivo!", 270, 207, 640)

	// Bloque 1 — código de acceso (PIN).
	drawAt(dst, bold, 26, th.label, "TU CÓDIGO DE ACCESO", 130, 400)
	drawAt(dst, bold, 110, th.pin, spacePIN(in.PIN), 124, 512)
	drawAt(dst, reg, 27, th.footer, "Entra rápido a Tinta, sin teclear tu correo.", 130, 566)

	// Divisor.
	fillRect(dst, image.Rect(130, 612, 950, 618), th.divider)

	// Bloque 2 — primeros pasos dentro del sistema.
	drawAt(dst, bold, 26, th.label, "YA PUEDES EMPEZAR", 130, 690)
	drawStep(dst, bold, th, "1", "Da de alta a tus socios", 752)
	drawStep(dst, bold, th, "2", "Registra tu primer cobro", 840)
	barcode(dst, 130, 950, 928, 30, th.pin)
}

// drawStep dibuja un paso numerado: círculo terracota con el número + texto.
func drawStep(dst *image.RGBA, bold *opentype.Font, th theme, num, text string, cyCircle int) {
	fillDiscAA(dst, 166, cyCircle, 26, th.label)
	drawCenterX(dst, bold, 30, th.bg, num, 166, cyCircle+11)
	drawAtFit(dst, bold, 30, th.fieldVal, text, 215, cyCircle+11, 760)
}

// copyFor returns label, field label, and footer lines per recipient kind.
//
// Socio (ADR-010): el código grande ES el NÚMERO DE SOCIO — identificador
// público, entero, único 1:1 por gym, que además sirve de credencial de
// check-in cuando el biométrico no está. Un solo concepto de identidad.
//
// Operador: sigue siendo el PIN DE ACCESO de login en recepción — concepto
// aparte (users BC), NO tocado por ADR-010.
func copyFor(in Input) (label, fieldLbl, footer1, footer2 string) {
	if in.Kind == KindOperator {
		return "PIN DE ACCESO", "OPERADOR", "Inicia sesión en el sistema del gym.", ""
	}
	return "NÚMERO DE SOCIO", "SOCIO",
		"Es tu número de socio para entrar al gym.",
		"Úsalo si tu biométrico no está disponible."
}

// pickDark elige tema claro/oscuro de forma determinista por contenido: varía
// entre socios pero es estable si el mismo mensaje se reintenta (mismo banner).
func pickDark(in Input) bool {
	h := fnv.New32a()
	_, _ = h.Write([]byte(in.GymName + "|" + in.Name + "|" + in.PIN))
	return h.Sum32()%2 == 1
}

// ── helpers de dibujo ───────────────────────────────────────────────────────

func fillRect(dst *image.RGBA, r image.Rectangle, c color.Color) {
	draw.Draw(dst, r, &image.Uniform{c}, image.Point{}, draw.Src)
}

// blendPix mezcla c sobre el pixel (x,y) con cobertura a∈[0,1] (anti-aliasing).
func blendPix(dst *image.RGBA, x, y int, c color.RGBA, a float64) {
	if a <= 0 || x < 0 || y < 0 || x >= canvasW || y >= canvasH {
		return
	}
	if a >= 1 {
		dst.SetRGBA(x, y, c)
		return
	}
	o := dst.RGBAAt(x, y)
	mix := func(n, p uint8) uint8 { return uint8(float64(n)*a + float64(p)*(1-a)) }
	dst.SetRGBA(x, y, color.RGBA{mix(c.R, o.R), mix(c.G, o.G), mix(c.B, o.B), 0xff})
}

// fillDiscAA dibuja un disco lleno con borde anti-aliased.
func fillDiscAA(dst *image.RGBA, cx, cy int, r float64, c color.RGBA) {
	x0, x1 := int(float64(cx)-r-1), int(float64(cx)+r+1)
	y0, y1 := int(float64(cy)-r-1), int(float64(cy)+r+1)
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			d := math.Hypot(float64(x-cx), float64(y-cy))
			if cov := r - d + 0.5; cov > 0 {
				if cov > 1 {
					cov = 1
				}
				blendPix(dst, x, y, c, cov)
			}
		}
	}
}

// fillRoundRect rellena un rectángulo con esquinas redondeadas (AA en esquinas).
func fillRoundRect(dst *image.RGBA, x0, y0, x1, y1 int, r int, c color.RGBA) {
	fillRect(dst, image.Rect(x0+r, y0, x1-r, y1), c)
	fillRect(dst, image.Rect(x0, y0+r, x1, y1-r), c)
	fillDiscAA(dst, x0+r, y0+r, float64(r), c)
	fillDiscAA(dst, x1-r-1, y0+r, float64(r), c)
	fillDiscAA(dst, x0+r, y1-r-1, float64(r), c)
	fillDiscAA(dst, x1-r-1, y1-r-1, float64(r), c)
}

// drawEllipseArc traza un arco elíptico con brocha AA.
func drawEllipseArc(dst *image.RGBA, cx, cy int, rx, ry, a0, a1, thick float64, c color.RGBA) {
	for a := a0; a <= a1; a += 0.0025 {
		x := float64(cx) + rx*math.Cos(a)
		y := float64(cy) + ry*math.Sin(a)
		fillDiscAA(dst, int(x), int(y), thick, c)
	}
}

// drawFingerprint dibuja un ícono de huella (elipses concéntricas con leve
// asimetría tipo "loop", abiertas abajo), anti-aliased.
func drawFingerprint(dst *image.RGBA, cx, cy, w, h int, c color.RGBA) {
	W, H := float64(w)/2, float64(h)/2
	rings := []struct{ fx, fy, a0, a1, dx float64 }{
		{1.00, 1.00, 0.80, 2.20, 0},
		{0.80, 0.84, 0.74, 2.26, 4},
		{0.61, 0.69, 0.68, 2.31, -3},
		{0.43, 0.54, 0.60, 2.37, 5},
		{0.26, 0.39, 0.50, 2.48, -2},
	}
	for _, r := range rings {
		drawEllipseArc(dst, cx+int(r.dx), cy, W*r.fx, H*r.fy, r.a0*math.Pi, r.a1*math.Pi, 2.1, c)
	}
}

// barcode dibuja un código de barras decorativo (patrón fijo).
func barcode(dst *image.RGBA, x0, x1, y, h int, c color.RGBA) {
	pat := []int{3, 2, 5, 2, 3, 6, 2, 4, 2, 3, 5, 2, 6, 2, 3, 2, 4, 3, 2, 5}
	x, i, bar := x0, 0, true
	for x < x1 {
		w := pat[i%len(pat)] * 4
		xe := x + w
		if xe > x1 {
			xe = x1
		}
		if bar {
			fillRect(dst, image.Rect(x, y, xe, y+h), c)
		}
		x, bar, i = xe, !bar, i+1
	}
}

// ── helpers de texto ────────────────────────────────────────────────────────

func newFace(f *opentype.Font, size float64) font.Face {
	face, _ := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	return face
}

func widthPx(f *opentype.Font, size float64, s string) float64 {
	face := newFace(f, size)
	defer face.Close()
	d := &font.Drawer{Face: face}
	return float64(d.MeasureString(s)) / 64
}

func drawAt(dst *image.RGBA, f *opentype.Font, size float64, c color.Color, s string, x, baseline int) {
	face := newFace(f, size)
	defer face.Close()
	d := &font.Drawer{Dst: dst, Src: &image.Uniform{c}, Face: face}
	d.Dot = fixed.Point26_6{X: fixed.I(x), Y: fixed.I(baseline)}
	d.DrawString(s)
}

// drawAtFit dibuja desde x reduciendo el tamaño si el texto excede maxW.
func drawAtFit(dst *image.RGBA, f *opentype.Font, size float64, c color.Color, s string, x, baseline int, maxW float64) {
	if w := widthPx(f, size, s); w > maxW {
		size = size * maxW / w
	}
	drawAt(dst, f, size, c, s, x, baseline)
}

func drawCenterX(dst *image.RGBA, f *opentype.Font, size float64, c color.Color, s string, cx, baseline int) {
	w := widthPx(f, size, s)
	drawAt(dst, f, size, c, s, cx-int(w/2), baseline)
}

func drawCentered(dst *image.RGBA, f *opentype.Font, size float64, c color.Color, s string, baseline int) {
	drawCenterX(dst, f, size, c, s, canvasW/2, baseline)
}

// ── utilidades ──────────────────────────────────────────────────────────────

// spacePIN renders "1234" as "1 2 3 4" for legibility at large sizes.
func spacePIN(pin string) string {
	out := make([]rune, 0, len(pin)*2)
	for i, r := range pin {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, r)
	}
	return string(out)
}

// initials toma hasta 2 iniciales del nombre del gym para el monograma.
func initials(s string) string {
	out := ""
	for _, p := range strings.Fields(s) {
		out += strings.ToUpper(string([]rune(p)[0]))
		if len(out) == 2 {
			break
		}
	}
	if out == "" {
		return "?"
	}
	return out
}

func orFallback(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
