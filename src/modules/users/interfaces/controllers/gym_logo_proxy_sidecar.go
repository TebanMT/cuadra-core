//go:build sidecar

package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// GymLogoSidecarProxy — POST/DELETE /api/v1/gyms/me/logo en el desktop.
//
// El logo es cloud-authoritative (SetGymLogo): el sidecar no tiene
// credenciales del bucket público, y un archivo en el disco de una sola
// recepción no le sirve al dashboard ni a otra computadora. Este proxy
// reenvía la subida tal cual al cloud con el sk_live_* del pareo — mismo
// patrón que WhatsAppSidecarProxy — y, si sale bien:
//
//   - refleja logo_url en la fila local de gyms (UPDATE quirúrgico, sin
//     version ni push: el dato vino DEL cloud), para que el ticket que se
//     imprima enseguida ya lo lleve;
//   - despierta al sync agent para traer la fila canónica (TouchGym +
//     logo_url en canonicalAugmentExpr).
//
// Subir o quitar el logo requiere internet; imprimir el ticket con logo no
// (logocache guarda la copia en disco la primera vez que lo descarga).
type GymLogoSidecarProxy struct {
	CloudURL   string
	HTTPClient *http.Client
	UoW        sharedDomain.UnitOfWork
	// Tokens valida el JWT LOCAL del operador.
	Tokens auth.TokenService
	// AgentReload dispara un tick del sync agent. Se asigna después de
	// construir el agent (cmd/sidecar); nil = esperar al tick normal.
	AgentReload func()
}

func NewGymLogoSidecarProxy(cfg GymLogoSidecarProxy) *GymLogoSidecarProxy {
	if cfg.HTTPClient == nil {
		// Más holgado que el ceremony de WhatsApp: viaja una imagen de
		// hasta 5 MB por el internet del gimnasio.
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	p := cfg
	return &p
}

func (p *GymLogoSidecarProxy) RegisterRoutes(r *gin.Engine) {
	g := r.Group("/api/v1/gyms/me/logo", middleware.AuthMiddleware(p.Tokens), middleware.RequireOwner())
	g.POST("", p.handleUpload)
	g.DELETE("", p.handleRemove)
}

func (p *GymLogoSidecarProxy) handleUpload(c *gin.Context) {
	// Tope del cuerpo: el archivo (maxLogoBytes) + el sobre del multipart.
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxLogoBytes+64<<10))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			abortJSON(c, http.StatusBadRequest, errLogoTooLarge.Error())
			return
		}
		abortJSON(c, http.StatusBadRequest, errLogoMissingFile.Error())
		return
	}
	p.forward(c, http.MethodPost, body, c.GetHeader("Content-Type"))
}

func (p *GymLogoSidecarProxy) handleRemove(c *gin.Context) {
	p.forward(c, http.MethodDelete, nil, "")
}

func (p *GymLogoSidecarProxy) forward(c *gin.Context, method string, body []byte, contentType string) {
	token := p.loadSidecarToken(c.Request.Context())
	if token == "" {
		abortJSON(c, http.StatusPreconditionFailed,
			"Esta computadora no está vinculada con la nube — vuelve a vincularla para cambiar el logo.")
		return
	}
	url := strings.TrimRight(p.CloudURL, "/") + "/api/v1/gyms/me/logo"
	req, err := http.NewRequestWithContext(c.Request.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		abortJSON(c, http.StatusInternalServerError, err.Error())
		return
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		abortJSON(c, http.StatusServiceUnavailable,
			"Necesitas internet para cambiar el logo. Revisa tu conexión e intenta de nuevo.")
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		abortJSON(c, http.StatusBadGateway, "respuesta de la nube ilegible")
		return
	}
	if resp.StatusCode == http.StatusOK {
		gymID, _ := middleware.GetGymID(c)
		p.mirrorLogo(c.Request.Context(), gymID, respBody)
		if p.AgentReload != nil {
			p.AgentReload()
		}
	}
	c.Data(resp.StatusCode, "application/json", respBody)
}

// mirrorLogo escribe en la fila local el logo_url que respondió el cloud
// (null al quitarlo). Best-effort, sin bump de version ni push — mismo
// razonamiento que mirrorConnected del proxy de WhatsApp: el apply del pull
// pisa después con el mismo valor.
func (p *GymLogoSidecarProxy) mirrorLogo(ctx context.Context, gymID uuid.UUID, cloudResp []byte) {
	if gymID == uuid.Nil {
		return
	}
	var env struct {
		Data struct {
			LogoURL *string `json:"logo_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(cloudResp, &env); err != nil {
		return
	}
	var logo any
	if env.Data.LogoURL != nil && strings.TrimSpace(*env.Data.LogoURL) != "" {
		logo = strings.TrimSpace(*env.Data.LogoURL)
	}
	_ = p.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx, ok := tx.(*sharedDomain.SqlxTransaction)
		if !ok {
			return nil
		}
		_, err := stx.Exec(ctx, `UPDATE gyms SET logo_url = ?, synced_at = ? WHERE id = ?`,
			logo, time.Now().UTC().UnixMilli(), gymID.String())
		return err
	})
}

// loadSidecarToken lee el sk_live_* del pareo. Vacío = sin vincular.
func (p *GymLogoSidecarProxy) loadSidecarToken(ctx context.Context) string {
	tx, err := p.UoW.Query(ctx)
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		return ""
	}
	st, err := syncShared.ReadState(ctx, tx)
	if err != nil {
		return ""
	}
	return st.SidecarToken
}

func abortJSON(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"status_code": status, "message": msg})
}
