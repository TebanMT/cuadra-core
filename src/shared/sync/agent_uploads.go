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

// photoEntity — descriptor estático por tipo de entidad que tiene foto
// sincronizable. Define las cuatro coordenadas que difieren entre
// members y products: tabla, columna, subdir/key path, presign kind.
// Los closures encapsulan la parte específica del enqueue (members y
// products tienen columnas NOT NULL distintas) y el clear-on-corrupt.
//
// Agregar una entidad nueva = declarar un nuevo var Entity de este tipo
// + wire en Agent.RunOnce (igual que se hizo para products).
type photoEntity struct {
	name             string // "member" / "product" — sólo para logs.
	table            string // "members" / "products".
	column           string // "photo_url" / "image_url".
	subdir           string // "members" / "products" — disk cache + key path.
	kind             string // "member_photo" / "product_photo" — presign req.
	buildPresignBody func(id, ext, contentType string) presignReqBody
	// replacePhotoURL — UPDATE local + version++ + synced_at=NULL,
	// re-leer NOT NULL cols, marshal + EnqueueSync. Entity-specific
	// porque los payloads del proyector difieren (members ≠ products).
	replacePhotoURL func(a *Agent, ctx context.Context, id, objectKey string) error
	// clearPhoto — zerorea el campo cuando la data URL es corrupta y
	// reintentar no tiene sentido. Misma motivación que en members.
	clearPhoto func(a *Agent, ctx context.Context, id string) error
}

var memberPhotoEntity = photoEntity{
	name:   "member",
	table:  "members",
	column: "photo_url",
	subdir: "members",
	kind:   "member_photo",
	buildPresignBody: func(id, ext, contentType string) presignReqBody {
		return presignReqBody{
			Kind:        "member_photo",
			MemberID:    id,
			Ext:         ext,
			ContentType: contentType,
		}
	},
	replacePhotoURL: (*Agent).replaceMemberPhotoURL,
	clearPhoto:      (*Agent).clearMemberPhoto,
}

var productPhotoEntity = photoEntity{
	name:   "product",
	table:  "products",
	column: "image_url",
	subdir: "products",
	kind:   "product_photo",
	buildPresignBody: func(id, ext, contentType string) presignReqBody {
		return presignReqBody{
			Kind:        "product_photo",
			ProductID:   id,
			Ext:         ext,
			ContentType: contentType,
		}
	},
	replacePhotoURL: (*Agent).replaceProductPhotoURL,
	clearPhoto:      (*Agent).clearProductPhoto,
}

// presignReqBody / presignRespBody — espejo de cloud's
// uploadPresignReq / uploadPresignResp en
// auth_controller.go. Si cambia ahí, cambiar aquí. MemberID y ProductID
// son mutuamente exclusivos según `kind` — omitempty para no mandar
// campos vacíos que el cloud podría malinterpretar.
type presignReqBody struct {
	Kind        string `json:"kind"`
	MemberID    string `json:"member_id,omitempty"`
	ProductID   string `json:"product_id,omitempty"`
	Ext         string `json:"ext"`
	ContentType string `json:"content_type"`
}

type presignRespBody struct {
	UploadURL string `json:"upload_url"`
	ObjectKey string `json:"object_key"`
}

// presignGetReqBody / presignGetRespBody — espejo del shape del
// endpoint cloud `POST /api/v1/uploads/presign-get`. El sidecar le
// pasa el object_key que ya tiene en SQLite (photo_url / image_url),
// recibe una GET firmada y baja los bytes a disco. Sin esto el GET
// retornaría 403 porque el bucket es privado.
type presignGetReqBody struct {
	ObjectKey string `json:"object_key"`
}

type presignGetRespBody struct {
	DownloadURL string `json:"download_url"`
}

// uploadPendingMemberPhotos — wrapper preservado por compatibilidad
// (Agent.RunOnce lo invoca por nombre). Delega a la versión genérica
// con el descriptor de members.
func (a *Agent) uploadPendingMemberPhotos(ctx context.Context) {
	a.uploadPendingPhotos(ctx, memberPhotoEntity)
}

// uploadPendingProductPhotos — análogo a uploadPendingMemberPhotos pero
// para products. Mismo ciclo (find data: → write disk → presign → PUT
// R2 → replace URL) sobre la tabla `products` y la columna `image_url`.
func (a *Agent) uploadPendingProductPhotos(ctx context.Context) {
	a.uploadPendingPhotos(ctx, productPhotoEntity)
}

// uploadPendingPhotos — corre antes de Push en cada tick.
// Encuentra filas donde la columna foto es un data: URL (foto recién
// elegida por el operador, todavía inline), extrae los bytes al cache
// local en disco, los sube a R2 vía el cloud (presign + PUT directo),
// y bumpea version + encola un push para que el cloud row guarde el
// object_key.
//
// El cache en disco (UploadsDir/<subdir>/<id>.<ext>) es lo que el FE
// renderea vía la ruta local-serve del sidecar — el FE nunca abre
// socket directo a R2. La función `downloadPendingPhotos` repuebla
// este cache desde R2 si el archivo falta (multi-device o post
// full-sync).
//
// Offline-first: si cualquier paso falla (cloud unreachable, R2 down,
// presign 501 si las env vars no están), el data: URL local sigue
// intacto y reintentamos en el próximo tick. Si el extract-a-disco
// ya sucedió pero el R2 PUT falló, el archivo en disco persiste y
// la URL data: queda en la fila — el siguiente tick reintentará
// el upload pero el FE ya ve la foto via local-serve.
//
// No registra failures contra el sync state agregado — esos son
// reservados para errores de push/pull que afectan TODA la sync. Una
// foto que no sube no debe pintar el indicator en rojo.
func (a *Agent) uploadPendingPhotos(ctx context.Context, e photoEntity) {
	token := a.currentToken()
	if token == "" {
		a.cfg.Logger.Printf("[upload] skip: no sidecar token (sin login todavía)")
		return
	}
	pending, err := a.findPendingPhotos(ctx, e)
	if err != nil {
		a.cfg.Logger.Printf("[upload] %s find pending: %v", e.name, err)
		return
	}
	if len(pending) == 0 {
		return
	}
	a.cfg.Logger.Printf("[upload] %d foto(s) de %s pendiente(s) — subiendo a R2", len(pending), e.name)
	for _, p := range pending {
		if err := a.uploadOnePhoto(ctx, token, e, p); err != nil {
			// Log only — los reintentos pasan en el próximo tick.
			a.cfg.Logger.Printf("[upload] %s %s: %v", e.name, p.EntityID, err)
			continue
		}
		a.cfg.Logger.Printf("[upload] %s %s: OK", e.name, p.EntityID)
	}
}

type pendingPhoto struct {
	EntityID string
	DataURL  string
}

func (a *Agent) findPendingPhotos(ctx context.Context, e photoEntity) ([]pendingPhoto, error) {
	rows := []struct {
		ID       string `db:"id"`
		PhotoURL string `db:"photo_url"`
	}{}
	tx, err := a.uow.Query(ctx)
	if err != nil {
		return nil, err
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	// SQL building con fmt.Sprintf es seguro acá: table/column vienen
	// del descriptor estático (no input de usuario). El alias `photo_url`
	// mantiene fijo el shape sqlx aunque la columna se llame distinto
	// en cada tabla.
	// LIMIT acotado: drainamos lentamente para no bloquear el tick.
	q := fmt.Sprintf(`
		SELECT id, %s AS photo_url FROM %s
		WHERE %s LIKE 'data:%%'
		  AND deleted_at IS NULL
		LIMIT ?`, e.column, e.table, e.column)
	if err := stx.Select(ctx, &rows, q, uploadBatchLimit); err != nil {
		return nil, err
	}
	out := make([]pendingPhoto, len(rows))
	for i, r := range rows {
		out[i] = pendingPhoto{EntityID: r.ID, DataURL: r.PhotoURL}
	}
	return out, nil
}

func (a *Agent) uploadOnePhoto(ctx context.Context, token string, e photoEntity, p pendingPhoto) error {
	contentType, body, err := parseDataURL(p.DataURL)
	if err != nil {
		// Data URL corrupta — no vale la pena seguir reintentándola.
		// Limpiamos el campo para que el dueño pueda re-elegir foto.
		a.cfg.Logger.Printf("[upload] %s %s: corrupt data URL, clearing — %v", e.name, p.EntityID, err)
		return e.clearPhoto(a, ctx, p.EntityID)
	}
	ext := extFromContentType(contentType)
	if ext == "" {
		return fmt.Errorf("unsupported content type %q", contentType)
	}

	// Paso 1 — extraer a disco ANTES de hablar con la nube. Esto
	// desacopla el cache del éxito del upload: aunque R2 esté caído,
	// el FE ya puede renderear la foto via local-serve. Idempotente
	// (re-escribir mismos bytes es seguro).
	if err := a.writePhotoToDisk(e, p.EntityID, ext, body); err != nil {
		return fmt.Errorf("write disk: %w", err)
	}

	pres, err := a.requestPresign(ctx, token, e.buildPresignBody(p.EntityID, ext, contentType))
	if err != nil {
		return fmt.Errorf("presign: %w", err)
	}

	if err := putToR2(ctx, a.cfg.HTTPClient, pres.UploadURL, contentType, body); err != nil {
		return fmt.Errorf("R2 PUT: %w", err)
	}

	// Guardamos el object_key (NO una URL pública). El bucket es
	// privado, así que cualquier lectura posterior requiere una GET
	// firmada que pedimos al cloud — el key es lo único estable.
	if err := e.replacePhotoURL(a, ctx, p.EntityID, pres.ObjectKey); err != nil {
		return fmt.Errorf("update local: %w", err)
	}
	return nil
}

// writePhotoToDisk persiste los bytes en UploadsDir/<subdir>/<id>.<ext>.
// El archivo es leído por la ruta local-serve del sidecar para que el FE
// renderee sin tocar R2. No-op si UploadsDir está vacío (build de tests).
func (a *Agent) writePhotoToDisk(e photoEntity, entityID, ext string, body []byte) error {
	if a.cfg.UploadsDir == "" {
		return fmt.Errorf("uploads dir not configured")
	}
	dir := filepath.Join(a.cfg.UploadsDir, e.subdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Si existe una foto previa con otra extensión (jpg→png), la
	// barremos primero para evitar dos archivos para el mismo id.
	// La ruta local-serve escoge por glob de prefijo, así que dos
	// archivos generarían ambigüedad.
	if err := removeOtherPhotos(dir, entityID, ext); err != nil {
		// No-fatal: si el cleanup falla seguimos con el write nuevo;
		// el caller maneja la ambigüedad agarrando el primero.
		a.cfg.Logger.Printf("[upload] %s %s: stale photo cleanup: %v", e.name, entityID, err)
	}
	path := filepath.Join(dir, entityID+"."+ext)
	// WriteFile es atómico-ish vía O_TRUNC; suficiente para 1-2MB de imagen.
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return err
	}
	// Log con path absoluto para que el operador pueda verificar dónde
	// quedaron los bytes — útil si hay sospecha de que estén yendo a
	// /tmp o a un cwd no persistente.
	if abs, err := filepath.Abs(path); err == nil {
		a.cfg.Logger.Printf("[upload] %s %s: cached → %s (%d bytes)", e.name, entityID, abs, len(body))
	}
	return nil
}

// removeOtherPhotos borra archivos <subdir>/<id>.<otra-ext> que hayan
// quedado de un upload anterior con extensión distinta. Lo hacemos en
// el camino de escritura porque la ruta local-serve elige por glob y
// dos archivos generan resultados no determinísticos.
func removeOtherPhotos(dir, entityID, keepExt string) error {
	entries, err := filepath.Glob(filepath.Join(dir, entityID+".*"))
	if err != nil {
		return err
	}
	keep := entityID + "." + keepExt
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

// replaceProductPhotoURL — paralelo a replaceMemberPhotoURL pero contra
// la tabla `products` y la columna `image_url`. El payload incluye TODAS
// las columnas NOT NULL que enqueueProduct espera en
// modules/products/.../product_sqlite.go — sino el proyector cloud falla
// con 23502 NOT NULL violation en el primer INSERT del row.
//
// La columna conserva el nombre `image_url` por consistencia con el
// modelo de products preexistente; semánticamente guarda lo mismo que
// `members.photo_url`: un object_key R2 (gyms/<gym>/products/<id>.<ext>)
// para filas nuevas, y URLs públicas legacy para filas viejas.
func (a *Agent) replaceProductPhotoURL(ctx context.Context, productID, objectKey string) error {
	return a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx := tx.(*sharedDomain.SqlxTransaction)
		now := time.Now().UTC().UnixMilli()

		// 1. Update local row.
		if _, err := stx.Exec(ctx, `
			UPDATE products
			SET image_url = ?,
			    version = version + 1,
			    updated_at = ?,
			    synced_at = NULL
			WHERE id = ?`,
			objectKey, now, productID,
		); err != nil {
			return err
		}

		// 2. Re-read. Money se guarda en cents en SQLite (ADR-002 §2);
		//    el proyector cloud espera la misma representación entera
		//    en el payload — no convertimos aquí.
		var row struct {
			ID           string  `db:"id"`
			GymID        string  `db:"gym_id"`
			Version      int     `db:"version"`
			CreatedAt    int64   `db:"created_at"`
			UpdatedAt    int64   `db:"updated_at"`
			Name         string  `db:"name"`
			Price        int64   `db:"price"`
			Stock        int     `db:"stock"`
			StockMinimum int     `db:"stock_minimum"`
			Category     *string `db:"category"`
			ImageURL     string  `db:"image_url"`
			Active       bool    `db:"active"`
		}
		if err := stx.Get(ctx, &row, `
			SELECT id, gym_id, version, created_at, updated_at,
			       name, price, stock, stock_minimum, category, image_url, active
			FROM products WHERE id = ?`, productID,
		); err != nil {
			return err
		}

		// 3. Enqueue. Mismo shape que enqueueProduct +
		//    image_url poblado.
		payload, err := json.Marshal(map[string]any{
			"id":            row.ID,
			"gym_id":        row.GymID,
			"version":       row.Version,
			"created_at":    row.CreatedAt,
			"updated_at":    row.UpdatedAt,
			"name":          row.Name,
			"price":         row.Price,
			"stock":         row.Stock,
			"stock_minimum": row.StockMinimum,
			"category":      row.Category,
			"image_url":     row.ImageURL,
			"active":        row.Active,
		})
		if err != nil {
			return err
		}
		return stx.EnqueueSync(ctx, "products", row.ID, "upsert", payload, row.Version)
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

// clearProductPhoto — paralelo a clearMemberPhoto. Misma motivación:
// data URL inválida, drenamos el campo para que el operador pueda
// re-elegir foto sin loop de reintentos.
func (a *Agent) clearProductPhoto(ctx context.Context, productID string) error {
	return a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx := tx.(*sharedDomain.SqlxTransaction)
		_, err := stx.Exec(ctx, `
			UPDATE products SET image_url = NULL,
			                    version = version + 1,
			                    updated_at = ?,
			                    synced_at = NULL
			WHERE id = ?`,
			time.Now().UTC().UnixMilli(), productID,
		)
		return err
	})
}

// downloadPendingMemberPhotos — wrapper preservado por compatibilidad.
func (a *Agent) downloadPendingMemberPhotos(ctx context.Context) {
	a.downloadPendingPhotos(ctx, memberPhotoEntity)
}

// downloadPendingProductPhotos — análogo a downloadPendingMemberPhotos
// pero sobre `products`/`image_url`.
func (a *Agent) downloadPendingProductPhotos(ctx context.Context) {
	a.downloadPendingPhotos(ctx, productPhotoEntity)
}

// downloadPendingPhotos — corre después del Pull en cada tick.
// Cubre el caso multi-device: device A subió una foto, el cloud row
// guarda el object_key R2, device B pulleó el cambio, ahora device B
// tiene la columna foto poblada pero no tiene el archivo en disco.
// Sin esto, el FE de B mostraría placeholder permanente. La ruta
// local-serve es la única forma legítima que el FE tiene de pedir la
// imagen — no abre socket directo a R2.
//
// Bucket privado: el sidecar NO puede hacer GET plano a R2 (devolvería
// 403). En cada miss pide al cloud `POST /uploads/presign-get` con el
// object_key, recibe una GET firmada (TTL corto), y baja los bytes.
//
// Errores per-row se loggean y se reintenta en el próximo tick.
func (a *Agent) downloadPendingPhotos(ctx context.Context, e photoEntity) {
	if a.cfg.UploadsDir == "" {
		return
	}
	token := a.currentToken()
	if token == "" {
		return
	}
	pending, err := a.findPhotosNeedingDownload(ctx, e)
	if err != nil {
		a.cfg.Logger.Printf("[download] %s find pending: %v", e.name, err)
		return
	}
	for _, p := range pending {
		if err := a.downloadOnePhoto(ctx, token, e, p); err != nil {
			a.cfg.Logger.Printf("[download] %s %s: %v", e.name, p.EntityID, err)
		}
	}
}

type photoDownload struct {
	EntityID  string
	ObjectKey string // siempre key (parseado de URL si la fila es legacy)
}

func (a *Agent) findPhotosNeedingDownload(ctx context.Context, e photoEntity) ([]photoDownload, error) {
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
	// fmt.Sprintf con identificadores del descriptor estático — sin
	// input de usuario, sin riesgo de inyección.
	q := fmt.Sprintf(`
		SELECT id, %s AS photo_url FROM %s
		WHERE (%s LIKE 'gyms/%%' OR %s LIKE 'http%%')
		  AND deleted_at IS NULL
		LIMIT ?`, e.column, e.table, e.column, e.column)
	if err := stx.Select(ctx, &rows, q, downloadBatchLimit*10); err != nil {
		return nil, err
	}
	out := make([]photoDownload, 0, len(rows))
	dir := filepath.Join(a.cfg.UploadsDir, e.subdir)
	for _, r := range rows {
		if photoExists(dir, r.ID) {
			continue
		}
		key := normalizeObjectKey(r.PhotoURL, e.subdir)
		if key == "" {
			continue
		}
		out = append(out, photoDownload{EntityID: r.ID, ObjectKey: key})
		if len(out) >= downloadBatchLimit {
			break
		}
	}
	return out, nil
}

func photoExists(dir, entityID string) bool {
	matches, err := filepath.Glob(filepath.Join(dir, entityID+".*"))
	if err != nil {
		return false
	}
	return len(matches) > 0
}

func (a *Agent) downloadOnePhoto(ctx context.Context, token string, e photoEntity, p photoDownload) error {
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
	return a.writePhotoToDisk(e, p.EntityID, ext, body)
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
//   - "gyms/<gym>/<subdir>/<id>.<ext>"           → tal cual
//   - "https://host/gyms/<gym>/<subdir>/<id>.<ext>?qs" → strip
//     protocol + host + query → "gyms/<gym>/<subdir>/<id>.<ext>"
//
// Las filas legacy guardan el shape URL (cuando el bucket era público);
// las filas nuevas guardan el key directo. El cloud espera el key. El
// param `subdir` se usa solamente para defensa adicional: rechazamos
// keys que no caen bajo el subdir esperado del entity.
func normalizeObjectKey(s, subdir string) string {
	expectedMid := "/" + subdir + "/"
	if strings.HasPrefix(s, "gyms/") {
		if strings.Contains(s, expectedMid) {
			return s
		}
		return ""
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
	if !strings.Contains(path, expectedMid) {
		return ""
	}
	return path
}

// extFromObjectKey extrae la extensión del object key. Shape esperado:
// gyms/<gym>/<subdir>/<id>.<ext> con ext ∈ {jpg, png, webp}. Si el shape
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
