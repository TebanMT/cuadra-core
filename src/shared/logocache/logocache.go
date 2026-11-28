// Package logocache descarga, normaliza y cachea el logo del gym para los
// comprobantes (ticket impreso y PDF). El logo es una URL que el dueño
// escribe en su perfil, así que:
//
//   - Sin internet el ticket igual lleva logo: la copia normalizada queda en
//     disco (sidecar) y se sirve desde ahí mientras la URL no cambie.
//   - Es una URL arbitraria → la descarga está blindada contra SSRF: sólo
//     http(s), y el dial rechaza IPs privadas, loopback, link-local y
//     similares (verificado al conectar, también en redirects).
//   - Se normaliza a PNG de máximo 512 px de ancho: gofpdf sólo entiende
//     PNG/JPEG y la impresora térmica no gana nada con un logo de 4000 px.
package logocache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // decoders registrados para image.Decode
	_ "image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "golang.org/x/image/webp"
)

const (
	maxDownloadBytes = 3 << 20
	maxWidthPx       = 512
	fetchTimeout     = 6 * time.Second
	// failureBackoff evita re-descargar en cada ticket una URL rota o un
	// gym sin internet: el intento fallido se recuerda unos minutos.
	failureBackoff = 5 * time.Minute
)

// ErrNoLogo — el gym no tiene logo configurado, o no se pudo obtener y no
// hay copia previa. El comprobante sale sin logo; nunca es un error fatal.
var ErrNoLogo = errors.New("logo no disponible")

// Cache es seguro para uso concurrente. Dir vacío = sólo memoria (cloud).
type Cache struct {
	Dir    string
	client *http.Client

	mu       sync.Mutex
	mem      map[string][]byte    // key → PNG
	failedAt map[string]time.Time // key → último intento fallido
}

func New(dir string) *Cache {
	return &Cache{
		Dir:      strings.TrimSpace(dir),
		client:   safeClient(),
		mem:      map[string][]byte{},
		failedAt: map[string]time.Time{},
	}
}

// PNG devuelve el logo normalizado para (gym, url). Orden: memoria → disco →
// descarga. Si la descarga falla pero hay una copia previa de OTRA url del
// mismo gym, no se usa: un logo viejo en un comprobante formal confunde más
// de lo que ayuda.
func (c *Cache) PNG(ctx context.Context, gymID, rawURL string) ([]byte, error) {
	rawURL = strings.TrimSpace(rawURL)
	if c == nil || rawURL == "" || gymID == "" {
		return nil, ErrNoLogo
	}
	key := cacheKey(gymID, rawURL)

	c.mu.Lock()
	if b, ok := c.mem[key]; ok {
		c.mu.Unlock()
		return b, nil
	}
	if at, ok := c.failedAt[key]; ok && time.Since(at) < failureBackoff {
		c.mu.Unlock()
		if b := c.readDisk(key); b != nil {
			return b, nil
		}
		return nil, ErrNoLogo
	}
	c.mu.Unlock()

	if b := c.readDisk(key); b != nil {
		c.remember(key, b)
		return b, nil
	}

	b, err := c.fetch(ctx, rawURL)
	if err != nil {
		c.mu.Lock()
		c.failedAt[key] = time.Now()
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrNoLogo, err)
	}
	c.remember(key, b)
	c.writeDisk(gymID, key, b)
	return b, nil
}

func (c *Cache) remember(key string, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mem[key] = b
	delete(c.failedAt, key)
}

func cacheKey(gymID, rawURL string) string {
	sum := sha256.Sum256([]byte(gymID + "\x00" + rawURL))
	return hex.EncodeToString(sum[:16])
}

func (c *Cache) path(key string) string {
	return filepath.Join(c.Dir, key+".png")
}

func (c *Cache) readDisk(key string) []byte {
	if c.Dir == "" {
		return nil
	}
	b, err := os.ReadFile(c.path(key))
	if err != nil || len(b) == 0 {
		return nil
	}
	return b
}

// writeDisk guarda la copia nueva y borra las de URLs anteriores del mismo
// gym (un archivo índice por gym apunta a la llave vigente).
func (c *Cache) writeDisk(gymID, key string, b []byte) {
	if c.Dir == "" {
		return
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return
	}
	tmp := c.path(key) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, c.path(key)); err != nil {
		_ = os.Remove(tmp)
		return
	}
	index := filepath.Join(c.Dir, "gym-"+cacheKey(gymID, "")+".current")
	if prev, err := os.ReadFile(index); err == nil {
		if old := strings.TrimSpace(string(prev)); old != "" && old != key {
			_ = os.Remove(c.path(old))
		}
	}
	_ = os.WriteFile(index, []byte(key), 0o644)
}

func (c *Cache) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("url de logo inválida")
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "image/png,image/jpeg,image/webp,image/gif;q=0.8")
	req.Header.Set("User-Agent", "Tinta-receipts/1.0")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("logo respondió %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDownloadBytes {
		return nil, errors.New("logo demasiado grande")
	}
	return Normalize(raw)
}

// Normalize decodifica PNG/JPEG/GIF/WebP, aplana la transparencia sobre
// blanco (papel) y reduce a maxWidthPx de ancho. Exportado para pruebas.
func Normalize(raw []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("formato de logo no soportado: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > 40_000_000 {
		return nil, errors.New("dimensiones de logo inválidas")
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > maxWidthPx {
		h = int(float64(h) * float64(maxWidthPx) / float64(w))
		if h < 1 {
			h = 1
		}
		w = maxWidthPx
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	// Muestreo por vecino más cercano: suficiente para un logo a 512 px y
	// sin dependencias extra.
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			sx := b.Min.X + x*b.Dx()/w
			r, g, bl, a := src.At(sx, sy).RGBA()
			if a == 0 {
				continue
			}
			// Composición sobre blanco (premultiplicado).
			inv := 0xffff - a
			dst.Set(x, y, color.RGBA64{
				R: uint16(r + inv),
				G: uint16(g + inv),
				B: uint16(bl + inv),
				A: 0xffff,
			})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// safeClient — cliente HTTP que sólo conecta a IPs públicas. La validación
// vive en el Control del dialer (después de resolver DNS), así que cubre
// DNS rebinding y redirects hacia la red interna.
func safeClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: fetchTimeout,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !publicIP(ip) {
				return fmt.Errorf("destino de logo no permitido: %s", host)
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   fetchTimeout,
		ResponseHeaderTimeout: fetchTimeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("demasiados redirects")
			}
			if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
				return errors.New("redirect no permitido")
			}
			return nil
		},
	}
}

func publicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10 (CGNAT) y 0.0.0.0/8 no son destinos públicos.
		if v4[0] == 100 && v4[1]&0xc0 == 64 {
			return false
		}
		if v4[0] == 0 {
			return false
		}
	}
	return true
}
