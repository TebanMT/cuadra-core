package controllers

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// extractDataURLToDisk decodifica una data URL (lo que el FE manda al
// crear/actualizar un socio con foto recién elegida) y la guarda en
// uploadsDir/members/<memberID>.<ext>. Esto se hace SÍNCRONAMENTE en
// el request handler para que cuando el FE reciba la respuesta y
// re-renderee la foto vía la ruta local-serve del sidecar, el archivo
// ya esté en disco — sin el lag del próximo tick del sync agent
// (hasta 30s).
//
// El upload a R2 sigue ocurriendo después en el agent (offline-first,
// retry-friendly). Aquí sólo nos ocupamos del cache local.
//
// No-op (return nil) cuando:
//   - uploadsDir está vacío (build cloud, o sidecar dev sin UPLOADS_DIR),
//   - dataURL no comienza con "data:" (es una URL R2 que ya vino del
//     cloud, o vacía).
//
// Errores se devuelven al caller que decide si fallar el request o
// loggear y continuar (preferimos lo último — el agent reintentará).
func extractDataURLToDisk(uploadsDir, memberID, dataURL string) error {
	if uploadsDir == "" {
		return nil
	}
	if !strings.HasPrefix(dataURL, "data:") {
		return nil
	}
	contentType, body, err := parseDataURL(dataURL)
	if err != nil {
		return err
	}
	ext := extFromContentType(contentType)
	if ext == "" {
		return fmt.Errorf("unsupported content type %q", contentType)
	}
	dir := filepath.Join(uploadsDir, "members")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Limpia variantes con otra extensión (jpg→png) para que la ruta
	// local-serve no encuentre ambigüedad — coincide con la lógica
	// equivalente en el sync agent.
	matches, _ := filepath.Glob(filepath.Join(dir, memberID+".*"))
	keep := memberID + "." + ext
	for _, p := range matches {
		if filepath.Base(p) != keep {
			_ = os.Remove(p)
		}
	}
	return os.WriteFile(filepath.Join(dir, keep), body, 0o644)
}

// parseDataURL — espejo de la versión sidecar-tagged en agent_uploads.go.
// Duplicada conscientemente para mantener este path sin build tag y
// disponible tanto desde el sidecar como desde tests del controller.
// Si el formato cambia, cambiar en ambos lados.
func parseDataURL(s string) (contentType string, body []byte, err error) {
	const prefix = "data:"
	if !strings.HasPrefix(s, prefix) {
		return "", nil, fmt.Errorf("not a data URL")
	}
	rest := s[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", nil, fmt.Errorf("missing comma separator")
	}
	meta, payload := rest[:comma], rest[comma+1:]
	semicolon := strings.IndexByte(meta, ';')
	if semicolon < 0 {
		return "", nil, fmt.Errorf("expected ;base64 marker")
	}
	contentType = meta[:semicolon]
	encoding := meta[semicolon+1:]
	if encoding != "base64" {
		return "", nil, fmt.Errorf("unsupported encoding %q", encoding)
	}
	body, err = base64.StdEncoding.DecodeString(payload)
	return contentType, body, err
}

func extFromContentType(ct string) string {
	switch ct {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	}
	return ""
}
