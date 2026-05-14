//go:build sidecar

package sync

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// uploadBatchLimit — máximo de fotos que el agent sube por tick. Lo
// mantengo bajo porque cada upload es una network call sincronizable
// (presign + PUT a R2). Si hay 20 pendientes drain en ~4 ticks.
const uploadBatchLimit = 5

// downloadBatchLimit — paralelo bajo: cada miss es un GET a R2. El
// caso típico es 1-2 fotos rezagadas tras un pull; no hay razón para
// agresividad.
const downloadBatchLimit = 5

// presignReqBody / presignRespBody — espejo de cloud's
// uploadPresignReq / uploadPresignResp en
// auth_controller.go. Si cambia ahí, cambiar aquí.
type presignReqBody struct {
	Kind        string `json:"kind"`
	MemberID    string `json:"member_id"`
	Ext         string `json:"ext"`
	ContentType string `json:"content_type"`
}

type presignRespBody struct {
	UploadURL string `json:"upload_url"`
	ObjectKey string `json:"object_key"`
}

// presignGetReqBody / presignGetRespBody — espejo del shape del
// endpoint cloud `POST /api/v1/uploads/presign-get`. El sidecar le
// pasa el object_key que ya tiene en SQLite (photo_url), recibe una
// GET firmada y baja los bytes a disco. Sin esto el GET retornaría
// 403 porque el bucket es privado.
type presignGetReqBody struct {
	ObjectKey string `json:"object_key"`
}

type presignGetRespBody struct {
	DownloadURL string `json:"download_url"`
}

// uploadPendingMemberPhotos — corre antes de Push en cada tick.
// Encuentra filas de members donde photo_url es un data: URL (foto
// recién elegida por el operador, todavía inline), extrae los bytes
// al cache local en disco, los sube a R2 vía el cloud (presign + PUT
// directo), y bumpea version + encola un push para que el cloud row
// guarde la URL pública.
//
// El cache en disco (UploadsDir/members/<id>.<ext>) es lo que el FE
// renderea vía la ruta local-serve del sidecar — el FE nunca abre
// socket directo a R2. La función `downloadPendingMemberPhotos`
// repuebla este cache desde R2 si el archivo falta (multi-device
// o post full-sync).
//
// Offline-first: si cualquier paso falla (cloud unreachable, R2 down,
// presign 501 si las env vars no están), el data: URL local sigue
// intacto y reintentamos en el próximo tick. Si el extract-a-disco
// ya sucedió pero el R2 PUT falló, el archivo en disco persiste y
// la URL data: queda en photo_url — el siguiente tick reintentará
// el upload pero el FE ya ve la foto via local-serve.
//
// No registra failures contra el sync state agregado — esos son
// reservados para errores de push/pull que afectan TODA la sync. Una
// foto que no sube no debe pintar el indicator en rojo.
func (a *Agent) uploadPendingMemberPhotos(ctx context.Context) {
	token := a.currentToken()
	if token == "" {
		a.cfg.Logger.Printf("[upload] skip: no sidecar token (sin login todavía)")
		return
	}
	pending, err := a.findPendingPhotos(ctx)
	if err != nil {
		a.cfg.Logger.Printf("[upload] find pending: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	a.cfg.Logger.Printf("[upload] %d foto(s) pendiente(s) — subiendo a R2", len(pending))
	for _, p := range pending {
		if err := a.uploadOnePhoto(ctx, token, p); err != nil {
			// Log only — los reintentos pasan en el próximo tick.
			a.cfg.Logger.Printf("[upload] member %s: %v", p.MemberID, err)
			continue
		}
		a.cfg.Logger.Printf("[upload] member %s: OK", p.MemberID)
	}
}

type pendingPhoto struct {
	MemberID string
	DataURL  string
}

func (a *Agent) findPendingPhotos(ctx context.Context) ([]pendingPhoto, error) {
	rows := []struct {
		ID       string `db:"id"`
		PhotoURL string `db:"photo_url"`
	}{}
	tx, err := a.uow.Query(ctx)
	if err != nil {
		return nil, err
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	// LIMIT acotado: drainamos lentamente para no bloquear el tick.
	if err := stx.Select(ctx, &rows, `
		SELECT id, photo_url FROM members
		WHERE photo_url LIKE 'data:%'
		  AND deleted_at IS NULL
		LIMIT ?`, uploadBatchLimit,
	); err != nil {
		return nil, err
	}
	out := make([]pendingPhoto, len(rows))
	for i, r := range rows {
		out[i] = pendingPhoto{MemberID: r.ID, DataURL: r.PhotoURL}
	}
	return out, nil
}

func (a *Agent) uploadOnePhoto(ctx context.Context, token string, p pendingPhoto) error {
	contentType, body, err := parseDataURL(p.DataURL)
	if err != nil {
		// Data URL corrupta — no vale la pena seguir reintentándola.
		// Limpiamos el campo para que el dueño pueda re-elegir foto.
		a.cfg.Logger.Printf("[upload] member %s: corrupt data URL, clearing — %v", p.MemberID, err)
		return a.clearMemberPhoto(ctx, p.MemberID)
	}
	ext := extFromContentType(contentType)
	if ext == "" {
		return fmt.Errorf("unsupported content type %q", contentType)
	}

	// Paso 1 — extraer a disco ANTES de hablar con la nube. Esto
	// desacopla el cache del éxito del upload: aunque R2 esté caído,
	// el FE ya puede renderear la foto via local-serve. Idempotente
	// (re-escribir mismos bytes es seguro).
	if err := a.writeMemberPhotoToDisk(p.MemberID, ext, body); err != nil {
		return fmt.Errorf("write disk: %w", err)
	}

	pres, err := a.requestPresign(ctx, token, presignReqBody{
		Kind:        "member_photo",
		MemberID:    p.MemberID,
		Ext:         ext,
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("presign: %w", err)
	}

	if err := putToR2(ctx, a.cfg.HTTPClient, pres.UploadURL, contentType, body); err != nil {
		return fmt.Errorf("R2 PUT: %w", err)
	}

	// Guardamos el object_key (NO una URL pública). El bucket es
	// privado, así que cualquier lectura posterior requiere una GET
	// firmada que pedimos al cloud — el key es lo único estable.
	if err := a.replaceMemberPhotoURL(ctx, p.MemberID, pres.ObjectKey); err != nil {
		return fmt.Errorf("update local: %w", err)
	}
	return nil
}

// writeMemberPhotoToDisk persiste los bytes en UploadsDir/members/<id>.<ext>.
// El archivo es leído por la ruta local-serve del sidecar para que el FE
// renderee sin tocar R2. No-op si UploadsDir está vacío (build de tests).
func (a *Agent) writeMemberPhotoToDisk(memberID, ext string, body []byte) error {
	if a.cfg.UploadsDir == "" {
		return fmt.Errorf("uploads dir not configured")
	}
	dir := filepath.Join(a.cfg.UploadsDir, "members")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Si existe una foto previa con otra extensión (jpg→png), la
	// barremos primero para evitar dos archivos para el mismo
	// member_id. La ruta local-serve escoge por glob de prefijo, así
	// que dos archivos generarían ambigüedad.
	if err := removeOtherMemberPhotos(dir, memberID, ext); err != nil {
		// No-fatal: si el cleanup falla seguimos con el write nuevo;
		// el caller maneja la ambigüedad agarrando el primero.
		a.cfg.Logger.Printf("[upload] member %s: stale photo cleanup: %v", memberID, err)
	}
	path := filepath.Join(dir, memberID+"."+ext)
	// WriteFile es atómico-ish vía O_TRUNC; suficiente para 1-2MB de imagen.
	return os.WriteFile(path, body, 0o644)
}

// removeOtherMemberPhotos borra archivos members/<id>.<otra-ext> que
// hayan quedado de un upload anterior con extensión distinta. Lo
// hacemos en el camino de escritura porque la ruta local-serve elige
// por glob y dos archivos generan resultados no determinísticos.
func removeOtherMemberPhotos(dir, memberID, keepExt string) error {
	entries, err := filepath.Glob(filepath.Join(dir, memberID+".*"))
	if err != nil {
		return err
	}
	keep := memberID + "." + keepExt
	for _, p := range entries {
		if filepath.Base(p) == keep {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// requestPresign llama al cloud /api/v1/uploads/presign con el sk_live_*
// que el agent ya tiene. Mismo `BaseURL` y `HTTPClient` que push/pull.
func (a *Agent) requestPresign(ctx context.Context, token string, body presignReqBody) (presignRespBody, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return presignRespBody{}, err
	}
	url := strings.TrimRight(a.cfg.BaseURL, "/") + "/api/v1/uploads/presign"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(buf))
	if err != nil {
		return presignRespBody{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return presignRespBody{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return presignRespBody{}, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}
	// El cloud envuelve {status_code, data, message}. Desempacar.
	var env struct {
		Data presignRespBody `json:"data"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		return presignRespBody{}, err
	}
	if env.Data.UploadURL == "" {
		// Algunas rutas devuelven el body sin envelope.
		if err := json.Unmarshal(respBody, &env.Data); err != nil {
			return presignRespBody{}, err
		}
	}
	return env.Data, nil
}

// putToR2 — PUT raw bytes al upload_url presignado. El Content-Type
// debe coincidir con el que vinculamos en el presign; si no, R2 falla
// la validación de signature.
func putToR2(ctx context.Context, client *http.Client, uploadURL, contentType string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("R2 returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// replaceMemberPhotoURL escribe el object_key R2 en la fila local,
// bumpea version + updated_at, y encola el cambio en sync_queue para
// que el próximo push lo propague a cloud. Todo en una sola Command
// transaction → atómico.
//
// Nota arquitectónica: la columna se llama `photo_url` por razones
// históricas, pero AHORA guarda el object_key (`gyms/<gym>/members/
// <id>.<ext>`), no una URL. El bucket R2 es privado — cualquier
// lectura requiere una GET firmada del cloud. El FE jamás lee este
// campo: usa la ruta local-serve del sidecar contra el cache en
// disco. El key es lo que el download task usa para pedir signed
// GETs cuando hay un miss de cache (multi-device).
//
// (Filas pre-existentes con URL completa siguen funcionando: el
// download task parsea el path para recuperar el key. Las filas
// nuevas guardan el key directo.)
func (a *Agent) replaceMemberPhotoURL(ctx context.Context, memberID, objectKey string) error {
	return a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx := tx.(*sharedDomain.SqlxTransaction)
		now := time.Now().UTC().UnixMilli()

		// 1. Update local row.
		if _, err := stx.Exec(ctx, `
			UPDATE members
			SET photo_url = ?,
			    version = version + 1,
			    updated_at = ?,
			    synced_at = NULL
			WHERE id = ?`,
			objectKey, now, memberID,
		); err != nil {
			return err
		}

		// 2. Re-read para obtener todos los campos NOT NULL que el
		//    payload de sync necesita (mismas columnas que
		//    enqueueMember en member_sqlite.go).
		var row struct {
			ID             string  `db:"id"`
			GymID          string  `db:"gym_id"`
			Version        int     `db:"version"`
			CreatedAt      int64   `db:"created_at"`
			UpdatedAt      int64   `db:"updated_at"`
			Folio          string  `db:"folio"`
			FullName       string  `db:"full_name"`
			Phone          string  `db:"phone"`
			Email          *string `db:"email"`
			Status         string  `db:"status"`
			EnrollmentPaid bool    `db:"enrollment_paid"`
			CreatedBy      string  `db:"created_by"`
			PhotoURL       string  `db:"photo_url"`
		}
		if err := stx.Get(ctx, &row, `
			SELECT id, gym_id, version, created_at, updated_at,
			       folio, full_name, phone, email, status,
			       enrollment_paid, created_by, photo_url
			FROM members WHERE id = ?`, memberID,
		); err != nil {
			return err
		}

		// 3. Enqueue. Mismo shape que enqueueMember + photo_url.
		payload, err := json.Marshal(map[string]any{
			"id":              row.ID,
			"gym_id":          row.GymID,
			"version":         row.Version,
			"created_at":      row.CreatedAt,
			"updated_at":      row.UpdatedAt,
			"folio":           row.Folio,
			"full_name":       row.FullName,
			"phone":           row.Phone,
			"email":           row.Email,
			"status":          row.Status,
			"enrollment_paid": row.EnrollmentPaid,
			"created_by":      row.CreatedBy,
			"photo_url":       row.PhotoURL,
		})
		if err != nil {
			return err
		}
		return stx.EnqueueSync(ctx, "members", row.ID, "upsert", payload, row.Version)
	})
}

// clearMemberPhoto se llama cuando la data URL local está corrupta y
// no podemos extraer bytes. Mejor zerorear que reintentar para siempre.
func (a *Agent) clearMemberPhoto(ctx context.Context, memberID string) error {
	return a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx := tx.(*sharedDomain.SqlxTransaction)
		_, err := stx.Exec(ctx, `
			UPDATE members SET photo_url = NULL,
			                    version = version + 1,
			                    updated_at = ?,
			                    synced_at = NULL
			WHERE id = ?`,
			time.Now().UTC().UnixMilli(), memberID,
		)
		return err
	})
}

// downloadPendingMemberPhotos — corre después del Pull en cada tick.
// Cubre el caso multi-device: device A subió una foto, el cloud row
// guarda el object_key R2, device B pulleó el cambio, ahora device B
// tiene photo_url=gyms/<gym>/members/<id>.<ext> pero no tiene el
// archivo en disco. Sin esto, el FE de B mostraría placeholder
// permanente. La ruta local-serve es la única forma legítima que el
// FE tiene de pedir la imagen — no abre socket directo a R2.
//
// Bucket privado: el sidecar NO puede hacer GET plano a R2 (devolvería
// 403). En cada miss pide al cloud `POST /uploads/presign-get` con el
// object_key, recibe una GET firmada (TTL corto), y baja los bytes.
//
// Errores per-row se loggean y se reintenta en el próximo tick.
func (a *Agent) downloadPendingMemberPhotos(ctx context.Context) {
	if a.cfg.UploadsDir == "" {
		return
	}
	token := a.currentToken()
	if token == "" {
		return
	}
	pending, err := a.findPhotosNeedingDownload(ctx)
	if err != nil {
		a.cfg.Logger.Printf("[download] find pending: %v", err)
		return
	}
	for _, p := range pending {
		if err := a.downloadOnePhoto(ctx, token, p); err != nil {
			a.cfg.Logger.Printf("[download] member %s: %v", p.MemberID, err)
		}
	}
}

type photoDownload struct {
	MemberID  string
	ObjectKey string // siempre key (parseado de URL si la fila es legacy)
}

func (a *Agent) findPhotosNeedingDownload(ctx context.Context) ([]photoDownload, error) {
	rows := []struct {
		ID       string `db:"id"`
		PhotoURL string `db:"photo_url"`
	}{}
	tx, err := a.uow.Query(ctx)
	if err != nil {
		return nil, err
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	// Filtra dos shapes: object keys nuevos ('gyms/%') y URLs legacy
	// de filas creadas pre-private-bucket ('http%'). data:% y NULL
	// quedan fuera. LIMIT generoso porque después filtramos por
	// existencia en disco; la mayoría de filas ya tendrán el archivo.
	if err := stx.Select(ctx, &rows, `
		SELECT id, photo_url FROM members
		WHERE (photo_url LIKE 'gyms/%' OR photo_url LIKE 'http%')
		  AND deleted_at IS NULL
		LIMIT ?`, downloadBatchLimit*10,
	); err != nil {
		return nil, err
	}
	out := make([]photoDownload, 0, len(rows))
	dir := filepath.Join(a.cfg.UploadsDir, "members")
	for _, r := range rows {
		if memberPhotoExists(dir, r.ID) {
			continue
		}
		key := normalizeObjectKey(r.PhotoURL)
		if key == "" {
			continue
		}
		out = append(out, photoDownload{MemberID: r.ID, ObjectKey: key})
		if len(out) >= downloadBatchLimit {
			break
		}
	}
	return out, nil
}

func memberPhotoExists(dir, memberID string) bool {
	matches, err := filepath.Glob(filepath.Join(dir, memberID+".*"))
	if err != nil {
		return false
	}
	return len(matches) > 0
}

func (a *Agent) downloadOnePhoto(ctx context.Context, token string, p photoDownload) error {
	ext := extFromObjectKey(p.ObjectKey)
	if ext == "" {
		return fmt.Errorf("can't infer ext from key %q", p.ObjectKey)
	}
	signed, err := a.requestPresignGet(ctx, token, p.ObjectKey)
	if err != nil {
		return fmt.Errorf("presign-get: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", signed, nil)
	if err != nil {
		return err
	}
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("R2 GET %d", resp.StatusCode)
	}
	// 10MB tope defensivo — fotos JPEG/PNG razonables están bien por
	// debajo. Si la URL apunta a algo gigante o malicioso, cortamos.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	return a.writeMemberPhotoToDisk(p.MemberID, ext, body)
}

// requestPresignGet llama al cloud /api/v1/uploads/presign-get. Mismo
// pattern que requestPresign (POST PUT) — token sk_live_*, JSON.
func (a *Agent) requestPresignGet(ctx context.Context, token, objectKey string) (string, error) {
	buf, err := json.Marshal(presignGetReqBody{ObjectKey: objectKey})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(a.cfg.BaseURL, "/") + "/api/v1/uploads/presign-get"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}
	var env struct {
		Data presignGetRespBody `json:"data"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		return "", err
	}
	if env.Data.DownloadURL == "" {
		if err := json.Unmarshal(respBody, &env.Data); err != nil {
			return "", err
		}
	}
	return env.Data.DownloadURL, nil
}

// normalizeObjectKey acepta dos shapes y devuelve un key plano:
//
//   - "gyms/<gym>/members/<id>.<ext>"           → tal cual
//   - "https://host/gyms/<gym>/members/<id>.<ext>?qs" → strip
//     protocol + host + query → "gyms/<gym>/members/<id>.<ext>"
//
// Las filas legacy guardan el shape URL (cuando el bucket era público);
// las filas nuevas guardan el key directo. El cloud espera el key.
func normalizeObjectKey(s string) string {
	if strings.HasPrefix(s, "gyms/") {
		return s
	}
	if !strings.HasPrefix(s, "http") {
		return ""
	}
	// Strip query.
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	// Strip protocol + host. URL shape: https://host/<path...>
	scheme := strings.Index(s, "://")
	if scheme < 0 {
		return ""
	}
	rest := s[scheme+3:]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return ""
	}
	path := strings.TrimPrefix(rest[slash+1:], "/")
	if !strings.HasPrefix(path, "gyms/") {
		return ""
	}
	return path
}

// extFromObjectKey extrae la extensión del object key. Shape esperado:
// gyms/<gym>/members/<id>.<ext> con ext ∈ {jpg, png, webp}. Si el shape
// cambia, falla seguro devolviendo "".
func extFromObjectKey(k string) string {
	dot := strings.LastIndexByte(k, '.')
	slash := strings.LastIndexByte(k, '/')
	if dot < 0 || dot < slash {
		return ""
	}
	ext := strings.ToLower(k[dot+1:])
	if !isAllowedExt(ext) {
		return ""
	}
	return ext
}

func isAllowedExt(ext string) bool {
	switch ext {
	case "jpg", "jpeg", "png", "webp":
		return true
	}
	return false
}

// parseDataURL — desempaca "data:image/jpeg;base64,/9j/4AAQS..." en
// (contentType, rawBytes). Rechaza si el formato no calza con lo que
// el FE produce vía FileReader.readAsDataURL.
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
	// meta = "image/jpeg;base64" — content type antes del primer ;.
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
	if err != nil {
		return "", nil, err
	}
	return contentType, body, nil
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
