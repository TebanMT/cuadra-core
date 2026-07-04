//go:build sidecar

package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	syncShared "github.com/cuadra/cuadra-core/src/shared/sync"
)

// WhatsAppSidecarProxy reimplementa el ceremony de UC-037
// (start/connect/disconnect) en el build sidecar como forwarder delgado al
// cloud — mismo patrón que SidecarAuthProxy en users. El sidecar dejó de ser
// autoridad sobre gyms.whatsapp_business_*: el ceremony necesita internet y
// un provider real (Twilio sólo existe cloud-side; el sidecar traía un mock
// stdout que "registraba" senders contra nada), y el consumidor del estado
// es el dispatcher cloud (UsesOwnWhatsAppNumber para resolver el sender).
// Conectar local era escribir en un lado que nadie lee y que el push jamás
// propagaba (enqueueGym omite estos campos a propósito).
//
// Auth en dos saltos: el request local se valida con el JWT del operador
// (firmado por el sidecar) + role=owner; el forward al cloud viaja con el
// sk_live_* del pareo, porque el cloud no acepta JWTs firmados por el
// sidecar. El gate cloud (requireOwnerOrSidecarToken) confía en ese token
// como credencial del dueño — este proxy es quien sostiene esa premisa al
// exigir owner ANTES de forwardear.
//
// El GET de status NO pasa por aquí: sigue siendo local (el controller
// compartido lo sirve del SQLite), para que la card de WhatsApp renderee
// sin internet. Los campos locales se refrescan por dos vías: el mirror
// optimista de este proxy tras un connect/disconnect exitoso, y el sync
// pull (TouchGym cloud-side + inyección de gymCanonicalAugmentExpr).
type WhatsAppSidecarProxy struct {
	CloudURL   string
	HTTPClient *http.Client
	UoW        sharedDomain.UnitOfWork
	// Tokens valida el JWT LOCAL del operador (mismo TokenService que el
	// resto de las rutas del sidecar).
	Tokens auth.TokenService
}

func NewWhatsAppSidecarProxy(cfg WhatsAppSidecarProxy) *WhatsAppSidecarProxy {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	p := cfg
	return &p
}

func (p *WhatsAppSidecarProxy) RegisterRoutes(r *gin.Engine) {
	g := r.Group("/api/v1", middleware.AuthMiddleware(p.Tokens), middleware.RequireOwner())
	g.POST("/gyms/me/whatsapp/start", p.handleStart)
	g.POST("/gyms/me/whatsapp/connect", p.handleConnect)
	g.DELETE("/gyms/me/whatsapp", p.handleDisconnect)
}

func (p *WhatsAppSidecarProxy) handleStart(c *gin.Context) {
	p.forwardCeremony(c, http.MethodPost, "/api/v1/gyms/me/whatsapp/start", nil)
}

func (p *WhatsAppSidecarProxy) handleConnect(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	p.forwardCeremony(c, http.MethodPost, "/api/v1/gyms/me/whatsapp/connect", func(status int, body []byte) {
		if status != http.StatusOK {
			return
		}
		p.mirrorConnected(c.Request.Context(), gymID, body)
	})
}

func (p *WhatsAppSidecarProxy) handleDisconnect(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	p.forwardCeremony(c, http.MethodDelete, "/api/v1/gyms/me/whatsapp", func(status int, body []byte) {
		if status != http.StatusOK {
			return
		}
		p.mirrorDisconnected(c.Request.Context(), gymID)
	})
}

// forwardCeremony relays the incoming request to the cloud, authenticated
// with the sidecar's sk_live_* credential, and pipes the response back
// verbatim (the cloud already speaks the envelope the FE expects). onSuccess
// (optional) runs before relaying so the local mirror is fresh by the time
// the FE invalidates its whatsapp query.
func (p *WhatsAppSidecarProxy) forwardCeremony(c *gin.Context, method, path string, onDone func(status int, body []byte)) {
	sidecarToken := p.loadSidecarToken(c.Request.Context())
	if sidecarToken == "" {
		// Sin pareo no hay credencial contra el cloud — mismo 412 que el
		// refresh de identidad del auth proxy.
		c.AbortWithStatusJSON(http.StatusPreconditionFailed, gin.H{
			"status_code": http.StatusPreconditionFailed,
			"message":     "Esta computadora no está vinculada con la nube — vuelve a vincularla para conectar WhatsApp.",
		})
		return
	}

	body, err := c.GetRawData()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"status_code": http.StatusBadRequest,
			"message":     "cuerpo inválido",
		})
		return
	}

	url := strings.TrimRight(p.CloudURL, "/") + path
	req, err := http.NewRequestWithContext(c.Request.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"status_code": http.StatusInternalServerError,
			"message":     err.Error(),
		})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sidecarToken)

	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		// Cloud inalcanzable. El ceremony REQUIERE internet por diseño
		// (misma decisión que checkout: la credencial y el provider viven
		// en la nube); no hay fallback local que ofrecer.
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"status_code": http.StatusServiceUnavailable,
			"message":     "Necesitas internet para conectar o desconectar WhatsApp. Revisa tu conexión e intenta de nuevo.",
		})
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{
			"status_code": http.StatusBadGateway,
			"message":     "respuesta de la nube ilegible",
		})
		return
	}
	if onDone != nil {
		onDone(resp.StatusCode, respBody)
	}
	c.Data(resp.StatusCode, "application/json", respBody)
}

// mirrorConnected refleja el connect exitoso en la fila local de gyms para
// que el GET de status (local) muestre "conectado" de inmediato, sin esperar
// el próximo pull. Best-effort y quirúrgico a propósito:
//   - raw UPDATE de sólo los campos whatsapp + synced_at — NO bumpea version
//     ni pasa por el repo (que encolaría un push echo vía enqueueGym);
//   - la fila canónica llega igual por sync (TouchGym bumpeó el journal) y
//     este mirror no compite con ella: el apply del pull pisa con lo mismo.
//
// Mismo razonamiento que mirrorCloudIdentity en el auth proxy: datos que
// vinieron DEL cloud no se re-empujan.
func (p *WhatsAppSidecarProxy) mirrorConnected(ctx context.Context, gymID uuid.UUID, cloudResp []byte) {
	if gymID == uuid.Nil {
		return
	}
	var env struct {
		Data struct {
			Phone       string    `json:"phone"`
			ConnectedAt time.Time `json:"connected_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(cloudResp, &env); err != nil || env.Data.Phone == "" {
		return
	}
	connectedAt := env.Data.ConnectedAt
	if connectedAt.IsZero() {
		connectedAt = time.Now().UTC()
	}
	_ = p.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx, ok := tx.(*sharedDomain.SqlxTransaction)
		if !ok {
			return nil
		}
		now := time.Now().UTC().UnixMilli()
		_, err := stx.Exec(ctx, `
			UPDATE gyms SET
			    whatsapp_business_phone = ?,
			    whatsapp_connected_at = ?,
			    synced_at = ?
			WHERE id = ?`,
			env.Data.Phone, connectedAt.UnixMilli(), now, gymID.String())
		return err
	})
}

// mirrorDisconnected limpia los campos whatsapp locales tras un disconnect
// exitoso en cloud. Misma semántica best-effort que mirrorConnected.
func (p *WhatsAppSidecarProxy) mirrorDisconnected(ctx context.Context, gymID uuid.UUID) {
	if gymID == uuid.Nil {
		return
	}
	_ = p.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx, ok := tx.(*sharedDomain.SqlxTransaction)
		if !ok {
			return nil
		}
		now := time.Now().UTC().UnixMilli()
		_, err := stx.Exec(ctx, `
			UPDATE gyms SET
			    whatsapp_business_phone = NULL,
			    whatsapp_business_token_enc = NULL,
			    whatsapp_connected_at = NULL,
			    synced_at = ?
			WHERE id = ?`,
			now, gymID.String())
		return err
	})
}

// loadSidecarToken lee el sk_live_* del sync_state — la credencial que el
// pareo dejó y que el sync agent usa en cada tick. Vacío = sidecar sin parear.
func (p *WhatsAppSidecarProxy) loadSidecarToken(ctx context.Context) string {
	tx, err := p.UoW.Query(ctx)
	if err != nil {
		return ""
	}
	st, err := syncShared.ReadState(ctx, tx)
	if err != nil {
		return ""
	}
	return st.SidecarToken
}
