// Package audithttp contiene el HTTP controller para GET /api/v1/audit-log.
// Vive aparte del paquete `audit` para evitar el ciclo de imports:
// `middleware` ya importa `audit` (para WithIP/WithUA), así que un
// controller dentro de `audit` no puede a su vez importar `middleware`.
package audithttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	usersRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// Controller expone GET /api/v1/audit-log. Owner-only — la bitácora del gym
// no debería ser visible para operadores (ven sus propias acciones reflejadas
// en las páginas de cada BC; el log completo es del dueño).
//
// Requiere `Reader` cableado al store correcto: NewPostgresReader() en cloud,
// NewSQLiteReader() en sidecar (build tags hacen el switch).
//
// Users es opcional — cuando está cableado, el controller resuelve los nombres
// de los actores en una sola pasada para evitar N+1; sin él, los rows
// devuelven actor_name vacío y el FE muestra "sistema" como fallback.
type Controller struct {
	Reader audit.Reader
	Users  usersRepo.UserRepository
	UoW    sharedDomain.UnitOfWork
	Tokens auth.TokenService
	// PlanGate (opcional) restringe la bitácora a Plus. Es feature de
	// auditoría avanzada — los gyms en Standard no la necesitan, y al
	// limitarla la respondemos del lado del FE con un upsell card.
	PlanGate gin.HandlerFunc
}

func NewController(reader audit.Reader, users usersRepo.UserRepository, uow sharedDomain.UnitOfWork, tokens auth.TokenService) *Controller {
	return &Controller{Reader: reader, Users: users, UoW: uow, Tokens: tokens}
}

func (c *Controller) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(c.Tokens))
	if c.PlanGate != nil {
		api.Use(c.PlanGate)
	}
	api.GET("/audit-log", middleware.RequireOwner(), c.handleList)
}

// auditEntryWire espeja AuditLogEntry del FE
// (cuadra-desktop/src/hooks/useAuditLog.ts). `changes` se devuelve como
// objeto JSON desempaquetado (no string) para que el FE muestre el diff
// pretty-printed sin tener que JSON.parse.
type auditEntryWire struct {
	ID         uuid.UUID      `json:"id"`
	GymID      uuid.UUID      `json:"gym_id"`
	EntityType string         `json:"entity_type"`
	EntityID   *uuid.UUID     `json:"entity_id"`
	Action     string         `json:"action"`
	ActorID    *uuid.UUID     `json:"actor_id"`
	ActorName  *string        `json:"actor_name"`
	Changes    map[string]any `json:"changes"`
	CreatedAt  time.Time      `json:"created_at"`
}

type auditListResp struct {
	Items    []auditEntryWire `json:"items"`
	Total    int              `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}

func (c *Controller) handleList(ctx *gin.Context) {
	gymID, ok := middleware.GetGymID(ctx)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(ctx, http.StatusUnauthorized, errors.New("autenticación requerida"))
		return
	}
	if c.Reader == nil || c.UoW == nil {
		utils.ErrorResponse(ctx, http.StatusInternalServerError, errors.New("audit reader no configurado"))
		return
	}
	page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(ctx.DefaultQuery("page_size", "50"))
	q := audit.ListQuery{
		GymID:      gymID,
		EntityType: ctx.Query("entity_type"),
		Page:       page,
		PageSize:   pageSize,
	}
	if raw := ctx.Query("actor_id"); raw != "" {
		if id, err := uuid.Parse(raw); err == nil {
			q.ActorID = &id
		}
	}
	// `from` y `to` llegan como YYYY-MM-DD del FE (el componente <input type="date">).
	// Tratamos `to` como inclusivo del día — el reader extiende +24h internamente.
	if raw := ctx.Query("from"); raw != "" {
		if t, err := time.Parse("2006-01-02", raw); err == nil {
			q.From = &t
		}
	}
	if raw := ctx.Query("to"); raw != "" {
		if t, err := time.Parse("2006-01-02", raw); err == nil {
			q.To = &t
		}
	}

	tx, err := c.UoW.Query(ctx.Request.Context())
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusInternalServerError, err)
		return
	}
	rows, total, err := c.Reader.List(tx, q)
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusInternalServerError, err)
		return
	}

	// Resolución de nombres en una sola pasada — N+1 sería costoso para
	// gyms con bitácoras largas. Si el reader ya popula ActorName (joined
	// query) lo respetamos; si vino vacío y tenemos repo, lo enriquecemos.
	nameByID := map[uuid.UUID]string{}
	if c.Users != nil {
		for _, e := range rows {
			if e.ActorID == nil || e.ActorName != "" {
				continue
			}
			if _, ok := nameByID[*e.ActorID]; ok {
				continue
			}
			u, err := c.Users.GetByID(tx, *e.ActorID)
			if err == nil && u != nil {
				nameByID[*e.ActorID] = u.FullName
			} else {
				nameByID[*e.ActorID] = ""
			}
		}
	}

	items := make([]auditEntryWire, 0, len(rows))
	for _, e := range rows {
		var actorName *string
		if e.ActorName != "" {
			v := e.ActorName
			actorName = &v
		} else if e.ActorID != nil {
			if v, ok := nameByID[*e.ActorID]; ok && v != "" {
				actorName = &v
			}
		}
		var changes map[string]any
		if len(e.Changes) > 0 {
			_ = json.Unmarshal(e.Changes, &changes)
		}
		items = append(items, auditEntryWire{
			ID:         e.ID,
			GymID:      e.GymID,
			EntityType: e.EntityType,
			EntityID:   e.EntityID,
			Action:     e.Action,
			ActorID:    e.ActorID,
			ActorName:  actorName,
			Changes:    changes,
			CreatedAt:  e.CreatedAt,
		})
	}

	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	utils.JsonResponse(ctx, http.StatusOK, auditListResp{
		Items: items, Total: total, Page: page, PageSize: pageSize,
	})
}
