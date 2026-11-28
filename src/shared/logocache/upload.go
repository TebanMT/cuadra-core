package logocache

import (
	"bytes"
	"errors"
	"image"
	"image/png"

	xdraw "golang.org/x/image/draw"
)

// maxUploadSidePx — lado mayor del logo que se publica. Suficiente para el
// encabezado del dashboard en pantallas retina y para el ticket (que de
// todos modos se normaliza a maxWidthPx al imprimirse).
const maxUploadSidePx = 1024

var (
	// ErrUnsupportedImage — el archivo no es una imagen PNG/JPEG/GIF/WebP.
	ErrUnsupportedImage = errors.New("formato no soportado: usa PNG, JPG o WebP")
	// ErrInvalidDimensions — imagen vacía o tan grande que decodificarla
	// sería un riesgo de memoria (bomba de descompresión).
	ErrInvalidDimensions = errors.New("la imagen del logo es demasiado grande o está vacía")
)

// PrepareUpload valida y re-codifica el logo que sube el dueño ANTES de
// publicarlo:
//
//   - decodifica PNG/JPEG/GIF/WebP y rechaza cualquier otra cosa;
//   - reduce a maxUploadSidePx de lado mayor con CatmullRom (un logo de
//     4000 px de la imprenta no sirve de nada en un ticket de 80 mm);
//   - CONSERVA la transparencia: el dashboard lo pinta sobre fondos
//     oscuros. El aplanado sobre blanco es cosa del ticket (Normalize);
//   - re-codifica a PNG, lo que tira metadatos EXIF (una foto del logo
//     tomada con el celular puede traer la ubicación) y cualquier carga
//     pegada al archivo original. Lo que se publica es siempre un PNG limpio.
func PrepareUpload(raw []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrUnsupportedImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > 40_000_000 {
		return nil, ErrInvalidDimensions
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrUnsupportedImage
	}
	b := src.Bounds()
	w, h := fitWithin(b.Dx(), b.Dy(), maxUploadSidePx)
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&out, dst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// fitWithin escala (w, h) para que el lado mayor no pase de limit,
// conservando la proporción. Nunca agranda.
func fitWithin(w, h, limit int) (int, int) {
	if w <= limit && h <= limit {
		return w, h
	}
	if w >= h {
		return limit, max(1, h*limit/w)
	}
	return max(1, w*limit/h), limit
}
