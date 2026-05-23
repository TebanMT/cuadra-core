package controllers

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// MemberController bundles UC-012..UC-017 + UC-032 + UC-046 partial.
type MemberController struct {
	Create     *memApp.CreateMember
	Update     *memApp.UpdateMember
	List       *memApp.ListMembers
	Detail     *memApp.GetMemberDetail
	Toggle     *memApp.ToggleMemberStatus
	LockExpiry *memApp.LockMembershipExpiry
	AssignPin  *memApp.AssignPin
	// ImportCSV es opcional: el wiring real lo inyecta cmd/server y
	// cmd/sidecar via WithImportCSV. Si queda nil, el endpoint responde
	// 503 — útil para tests que no exercisan importación.
	ImportCSV *memApp.ImportMembersFromCSV
	Tokens    auth.TokenService
	// UploadsDir es la raíz del cache local de blobs (igual variable
	// que la AgentConfig). Cuando está seteada, los handlers de create
	// y update extraen sincronámente data URLs a disco para que el FE
	// pueda renderizar la foto inmediatamente después de guardar, sin
	// esperar al próximo tick del sync agent. Vacío deshabilita el
	// cache (build cloud / dev sin UPLOADS_DIR).
	UploadsDir string
	// SyncTrigger nudgea al sync agent para que corra ya (sin esperar
	// el próximo tick de 30s). Usado tras crear/editar un socio con
	// foto, así el upload a R2 arranca inmediatamente y el operador
	// ve la foto en el bucket en segundos en lugar de medio minuto.
	// Opcional — cuando nil, el upload sale en el siguiente tick.
	SyncTrigger func()
}

// WithUploadsDir setea el cache local de fotos. Devuelve el mismo
// controller para encadenar al construir. Llamado desde cmd/sidecar
// con el mismo UPLOADS_DIR que usa el sync agent.
func (ctrl *MemberController) WithUploadsDir(dir string) *MemberController {
	ctrl.UploadsDir = dir
	return ctrl
}

// WithSyncTrigger setea el callback de trigger. Llamado desde
// cmd/sidecar con agent.TriggerNow.
func (ctrl *MemberController) WithSyncTrigger(fn func()) *MemberController {
	ctrl.SyncTrigger = fn
	return ctrl
}

// WithImportCSV cablea el use case de importación masiva (UC-046). Llamado
// desde cmd/server + cmd/sidecar tras construir el controller base.
func (ctrl *MemberController) WithImportCSV(uc *memApp.ImportMembersFromCSV) *MemberController {
	ctrl.ImportCSV = uc
	return ctrl
}

func NewMemberController(
	create *memApp.CreateMember,
	update *memApp.UpdateMember,
	list *memApp.ListMembers,
	detail *memApp.GetMemberDetail,
	toggle *memApp.ToggleMemberStatus,
	lockExpiry *memApp.LockMembershipExpiry,
	assignPin *memApp.AssignPin,
	tokens auth.TokenService,
) *MemberController {
	return &MemberController{
		Create: create, Update: update, List: list, Detail: detail,
		Toggle: toggle, LockExpiry: lockExpiry, AssignPin: assignPin, Tokens: tokens,
	}
}

func (ctrl *MemberController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		members := api.Group("/members")
		members.POST("", ctrl.handleCreate)
		members.GET("", ctrl.handleList)
		members.GET("/:id", ctrl.handleDetail)
		members.PATCH("/:id", ctrl.handleUpdate)
		members.PATCH("/:id/status", ctrl.handleToggleStatus)
		members.POST("/:id/pin", ctrl.handleAssignPin)

		// UC-046 — sólo el dueño puede importar masivo (DA-46.2). El gym
		// completo cambia con un click; ese poder no lo damos a operadores.
		members.POST("/import-csv", middleware.RequireOwner(), ctrl.handleImportCSV)

		// UC-017 — only owner can adjust expiry (DA-17.1).
		api.POST("/memberships/:id/lock-expiry", middleware.RequireOwner(), ctrl.handleLockExpiry)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type createMemberReq struct {
	FullName  string  `json:"full_name" validate:"required,min=3,max=100"`
	Phone     string  `json:"phone" validate:"required"`
	Email     *string `json:"email,omitempty"`
	Birthdate *string `json:"birthdate,omitempty"` // YYYY-MM-DD
	PhotoURL  *string `json:"photo_url,omitempty"`
	Notes     *string `json:"notes,omitempty"`
	// Gender opcional. omitempty + ptr para distinguir "campo no enviado"
	// (FE lo dejó vacío) de "se eligió Prefiero no decir". Valores válidos:
	// hombre, mujer, no_especificado. El handler valida via dominio.
	Gender              *string `json:"gender,omitempty"`
	MembershipTypeID    string  `json:"membership_type_id" validate:"required,uuid"`
	StartDate           string  `json:"start_date,omitempty"` // YYYY-MM-DD; defaults to today
	AllowDuplicatePhone bool    `json:"allow_duplicate_phone,omitempty"`
	ChargeFirstPayment  bool    `json:"charge_first_payment,omitempty"`
	ChargeEnrollment    bool    `json:"charge_enrollment,omitempty"`
	ChargeMaintenance   bool    `json:"charge_maintenance,omitempty"`
	EnrollmentAmount    float64 `json:"enrollment_amount,omitempty"`
	MaintenanceAmount   float64 `json:"maintenance_amount,omitempty"`
	PaymentMethod       string  `json:"payment_method,omitempty"`
}

type createMemberResp struct {
	MemberID     uuid.UUID `json:"member_id"`
	MembershipID uuid.UUID `json:"membership_id"`
	Folio        string    `json:"folio"`
	// ExpiryDate vacío cuando la membresía quedó en pending_payment.
	ExpiryDate          string `json:"expiry_date,omitempty"`
	MembershipStatus    string `json:"membership_status"`
	PendingFirstPayment bool   `json:"pending_first_payment"`
	// Pin del socio recién inscrito (auto-generado 4 dígitos único en el
	// gym). Siempre se envía para que el operador lo lea / escriba en la
	// credencial; vacío sólo en el caso muy raro de que el gym esté
	// saturado de PINs y se haya saltado la auto-asignación.
	Pin          string       `json:"pin,omitempty"`
	PinDispatch  *pinDispatch `json:"pin_dispatch,omitempty"`
	PaymentID    *uuid.UUID   `json:"payment_id,omitempty"`
	PaymentFolio string       `json:"payment_folio,omitempty"`
	PaymentTotal float64      `json:"payment_total,omitempty"`
}

// pinDispatch surfaces whether the welcome-PIN WhatsApp notification was
// enqueued. The FE renders one of three copies based on the shape:
//   - dispatched=true, recipient_phone="+52…"   → "PIN enviado a +52…"
//   - dispatched=false, skipped_reason="whatsapp_not_connected" / "no_member_phone"
//     → "Escríbelo en la credencial"
//   - dispatched=false, skipped_reason="disabled_by_gym"
//     → "Escríbelo en la credencial" (silencioso)
type pinDispatch struct {
	Dispatched     bool   `json:"dispatched"`
	SkippedReason  string `json:"skipped_reason,omitempty"`
	RecipientPhone string `json:"recipient_phone,omitempty"`
}

func toPinDispatch(d memApp.WelcomePinDispatchResult) *pinDispatch {
	if !d.Dispatched && d.SkippedReason == "" {
		return nil
	}
	return &pinDispatch{
		Dispatched:     d.Dispatched,
		SkippedReason:  d.SkippedReason,
		RecipientPhone: d.RecipientPhone,
	}
}

type updateMemberReq struct {
	FullName  *string `json:"full_name,omitempty"`
	Phone     *string `json:"phone,omitempty"`
	Email     *string `json:"email,omitempty"`
	Birthdate *string `json:"birthdate,omitempty"`
	PhotoURL  *string `json:"photo_url,omitempty"`
	Notes     *string `json:"notes,omitempty"`
	// Gender — nil = no change. ""  = limpiar (vuelve a NULL).
	Gender *string `json:"gender,omitempty"`
}

type toggleStatusReq struct {
	Status string `json:"status" validate:"required,oneof=active inactive lost"`
	Reason string `json:"reason,omitempty"`
}

type lockExpiryReq struct {
	Mode      string `json:"mode" validate:"required,oneof=extend set"`
	Days      int    `json:"days,omitempty"`
	NewExpiry string `json:"new_expiry,omitempty"` // YYYY-MM-DD when mode=set
	Reason    string `json:"reason" validate:"required,min=5,max=500"`
}

type assignPinReq struct {
	Pin string `json:"pin,omitempty"` // optional — empty = auto-generate
}

type memberResp struct {
	ID                  uuid.UUID `json:"id"`
	GymID               uuid.UUID `json:"gym_id"`
	Folio               string    `json:"folio"`
	FullName            string    `json:"full_name"`
	Phone               string    `json:"phone"`
	Email               *string   `json:"email,omitempty"`
	Birthdate           *string   `json:"birthdate,omitempty"`
	PhotoURL            *string   `json:"photo_url,omitempty"`
	Notes               *string   `json:"notes,omitempty"`
	Status              string    `json:"status"`
	EnrollmentPaid      bool      `json:"enrollment_paid"`
	LastMaintenancePaid *string   `json:"last_maintenance_paid,omitempty"`
	HasPin              bool      `json:"has_pin"`
	HasFingerprint      bool      `json:"has_fingerprint"`
	// Pin: el código de 4 dígitos visible para el operador (mismo
	// rationale que la migración 012 — texto plano, no es un secreto).
	// Vacío sólo cuando el socio no tiene PIN aún (caso histórico
	// pre-auto-assign o saturación de PINs en gym).
	Pin                  string     `json:"pin,omitempty"`
	LastContactAttemptAt *time.Time `json:"last_contact_attempt_at,omitempty"`
	// Gender siempre se envía (sin omitempty) para que el FE pueda
	// diferenciar entre "no cargado del backend" y "vacío". null en JSON
	// cuando el socio no tiene captura. NO se incluye en webhooks ni en
	// CSV export salvo opt-in explícito del dueño (LFPDPPP — dato personal).
	Gender    *string   `json:"gender"`
	CreatedAt time.Time `json:"created_at"`
}

type membershipResp struct {
	ID               uuid.UUID `json:"id"`
	MembershipTypeID uuid.UUID `json:"membership_type_id"`
	TypeName         string    `json:"type_name"`
	Price            float64   `json:"price"`
	StartDate        string    `json:"start_date"`
	// ExpiryDate vacío cuando la membresía está en pending_payment.
	ExpiryDate           string `json:"expiry_date,omitempty"`
	Status               string `json:"status"`
	DurationDaysSnapshot int    `json:"duration_days_snapshot"`
}

type memberDetailResp struct {
	Member            memberResp      `json:"member"`
	CurrentMembership *membershipResp `json:"current_membership,omitempty"`
	AccessStatus      string          `json:"access_status"`
}

type memberListItemResp struct {
	Member            memberResp      `json:"member"`
	CurrentMembership *membershipResp `json:"current_membership,omitempty"`
	AccessStatus      string          `json:"access_status"`
}

type memberListResp struct {
	Items    []memberListItemResp `json:"items"`
	Total    int                  `json:"total"`
	Page     int                  `json:"page"`
	PageSize int                  `json:"page_size"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (ctrl *MemberController) handleCreate(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	var req createMemberReq
	if !bindJSON(c, &req) {
		return
	}
	typeID, err := uuid.Parse(req.MembershipTypeID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	now := time.Now().UTC()
	startDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if req.StartDate != "" {
		t, err := time.Parse("2006-01-02", req.StartDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		startDate = t
	}
	var birthdate *time.Time
	if req.Birthdate != nil && *req.Birthdate != "" {
		t, err := time.Parse("2006-01-02", *req.Birthdate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		birthdate = &t
	}
	out, err := ctrl.Create.Execute(c.Request.Context(), memApp.CreateMemberInput{
		GymID: gymID, ActorUserID: userID,
		FullName:            req.FullName,
		Phone:               req.Phone,
		Email:               req.Email,
		Birthdate:           birthdate,
		PhotoURL:            req.PhotoURL,
		Notes:               req.Notes,
		Gender:              req.Gender,
		MembershipTypeID:    typeID,
		StartDate:           startDate,
		AllowDuplicatePhone: req.AllowDuplicatePhone,
		ChargeFirstPayment:  req.ChargeFirstPayment,
		ChargeEnrollment:    req.ChargeEnrollment,
		ChargeMaintenance:   req.ChargeMaintenance,
		EnrollmentAmount:    req.EnrollmentAmount,
		MaintenanceAmount:   req.MaintenanceAmount,
		PaymentMethod:       req.PaymentMethod,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	// Cache local sincrónico: si el FE mandó la foto como data URL,
	// la dejamos en disco ANTES de responder para que la próxima
	// fetch del FE (vía local-serve) la encuentre. El upload a R2
	// lo hace el sync agent en background. Errores aquí se loggean
	// pero no fallan el request — el agent reintentará.
	if req.PhotoURL != nil {
		if err := extractDataURLToDisk(ctrl.UploadsDir, out.MemberID.String(), *req.PhotoURL); err != nil {
			log.Printf("[members] photo disk cache (create %s): %v", out.MemberID, err)
		}
		if ctrl.SyncTrigger != nil {
			ctrl.SyncTrigger()
		}
	}
	utils.JsonResponse(c, http.StatusCreated, createMemberResp{
		MemberID:            out.MemberID,
		MembershipID:        out.MembershipID,
		Folio:               out.Folio,
		ExpiryDate:          formatOptionalDate(out.ExpiryDate),
		MembershipStatus:    out.MembershipStatus,
		PendingFirstPayment: out.PendingFirstPayment,
		Pin:                 out.Pin,
		PinDispatch:         toPinDispatch(out.PinDispatch),
		PaymentID:           out.PaymentID,
		PaymentFolio:        out.PaymentFolio,
		PaymentTotal:        out.PaymentTotal,
	})
}

func (ctrl *MemberController) handleUpdate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req updateMemberReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	var birthdate *time.Time
	if req.Birthdate != nil {
		if *req.Birthdate == "" {
			zero := time.Time{}
			birthdate = &zero
		} else {
			t, err := time.Parse("2006-01-02", *req.Birthdate)
			if err != nil {
				utils.ErrorResponse(c, http.StatusBadRequest, err)
				return
			}
			birthdate = &t
		}
	}
	out, err := ctrl.Update.Execute(c.Request.Context(), memApp.UpdateMemberInput{
		GymID: gymID, ActorUserID: userID, MemberID: id,
		FullName: req.FullName, Phone: req.Phone, Email: req.Email,
		Birthdate: birthdate, PhotoURL: req.PhotoURL, Notes: req.Notes,
		Gender: req.Gender,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	// Mismo cache local sincrónico que en handleCreate. Si el campo
	// no vino (req.PhotoURL == nil) o vino vacío (foto removida) no
	// hacemos nada — extractDataURLToDisk es no-op para no-data-URLs.
	if req.PhotoURL != nil {
		if err := extractDataURLToDisk(ctrl.UploadsDir, id.String(), *req.PhotoURL); err != nil {
			log.Printf("[members] photo disk cache (update %s): %v", id, err)
		}
		if ctrl.SyncTrigger != nil {
			ctrl.SyncTrigger()
		}
	}
	utils.JsonResponse(c, http.StatusOK, toMemberResp(out))
}

func (ctrl *MemberController) handleList(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "25"))
	sort := c.Query("sort")
	asc := c.Query("dir") != "desc"
	var planID *uuid.UUID
	if v := c.Query("plan_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			planID = &id
		}
	}
	out, err := ctrl.List.Execute(c.Request.Context(), memApp.ListMembersInput{
		GymID: gymID, Search: c.Query("q"), StatusFilter: c.Query("status"),
		PlanID: planID, Sort: sort, SortAscending: asc,
		Page: page, PageSize: pageSize,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]memberListItemResp, 0, len(out.Items))
	for _, it := range out.Items {
		items = append(items, memberListItemResp{
			Member:            toMemberResp(it.Member),
			CurrentMembership: toMembershipResp(it.CurrentMembership),
			AccessStatus:      string(it.AccessStatus),
		})
	}
	utils.JsonResponse(c, http.StatusOK, memberListResp{
		Items: items, Total: out.Total, Page: out.Page, PageSize: out.PageSize,
	})
}

func (ctrl *MemberController) handleDetail(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.Detail.Execute(c.Request.Context(), memApp.GetMemberDetailInput{GymID: gymID, MemberID: id})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	memberR := toMemberResp(out.Member)
	memberR.HasFingerprint = out.HasFingerprint
	utils.JsonResponse(c, http.StatusOK, memberDetailResp{
		Member:            memberR,
		CurrentMembership: toMembershipResp(out.CurrentMembership),
		AccessStatus:      string(out.AccessStatus),
	})
}

func (ctrl *MemberController) handleToggleStatus(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req toggleStatusReq
	if !bindJSON(c, &req) {
		return
	}
	out, err := ctrl.Toggle.Execute(c.Request.Context(), memApp.ToggleMemberStatusInput{
		GymID: gymID, ActorUserID: userID, MemberID: id,
		NewStatus: req.Status, Reason: req.Reason,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toMemberResp(out))
}

func (ctrl *MemberController) handleLockExpiry(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req lockExpiryReq
	if !bindJSON(c, &req) {
		return
	}
	in := memApp.LockMembershipExpiryInput{
		GymID: gymID, ActorUserID: userID, MembershipID: id,
		Reason: req.Reason,
	}
	switch req.Mode {
	case "extend":
		in.Mode = memApp.ModeExtendDays
		in.Days = req.Days
	case "set":
		in.Mode = memApp.ModeSetDate
		t, err := time.Parse("2006-01-02", req.NewExpiry)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		in.NewExpiry = t
	}
	out, err := ctrl.LockExpiry.Execute(c.Request.Context(), in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{
		"membership_id":   out.MembershipID,
		"previous_expiry": out.PreviousExpiry.Format("2006-01-02"),
		"new_expiry":      out.NewExpiry.Format("2006-01-02"),
		"days_added":      out.DaysAdded,
	})
}

func (ctrl *MemberController) handleAssignPin(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req assignPinReq
	_ = c.ShouldBindJSON(&req)
	out, err := ctrl.AssignPin.Execute(c.Request.Context(), memApp.AssignPinInput{
		GymID: gymID, ActorUserID: userID, MemberID: id, PlainPin: req.Pin,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	resp := gin.H{"member_id": out.MemberID, "pin": out.Pin}
	if d := toPinDispatch(out.PinDispatch); d != nil {
		resp["pin_dispatch"] = d
	}
	utils.JsonResponse(c, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// UC-046 — importación masiva de socios + membresías desde CSV
// ---------------------------------------------------------------------------

// maxCSVImportBytes acota la carga del wizard. 5 MB cubre ~50K filas
// (CSV con socio + membresía ≈ 100 bytes/row). Más que eso huele a
// archivo equivocado, no a un gym de barrio.
const maxCSVImportBytes int64 = 5 * 1024 * 1024

type importCSVSummary struct {
	ImportedCount int  `json:"imported_count"`
	SkippedCount  int  `json:"skipped_count"`
	ErrorsCount   int  `json:"errors_count"`
	TotalDataRows int  `json:"total_data_rows"`
	Atomic        bool `json:"atomic"`
}

type importCSVResp struct {
	Imported []memApp.ImportedRow `json:"imported"`
	Skipped  []memApp.SkipRow     `json:"skipped"`
	Errors   []memApp.ErrorRow    `json:"errors"`
	Summary  importCSVSummary     `json:"summary"`
}

// handleImportCSV procesa multipart/form-data { file: <csv>, allow_duplicates: bool }.
// El query param ?allow_duplicates=true|false toma precedencia si está presente —
// el FE lo manda así para simplificar la mutation (no tiene que crear un field
// extra en el form).
func (ctrl *MemberController) handleImportCSV(c *gin.Context) {
	if ctrl.ImportCSV == nil {
		utils.ErrorResponse(c, http.StatusServiceUnavailable, errors.New("importación no configurada"))
		return
	}
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)

	fh, err := c.FormFile("file")
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, memErrors.ErrCSVMissingHeader)
		return
	}
	if fh.Size > maxCSVImportBytes {
		utils.ErrorResponse(c, http.StatusBadRequest, memErrors.ErrCSVTooLarge)
		return
	}
	// Content-Type del multipart header es informativo; lo chequeamos pero
	// no exclusivamente — Excel a veces manda application/vnd.ms-excel o
	// text/plain. Lo importante es que el contenido parseé bien como CSV;
	// la extensión .csv en filename y el header MIME los chequeamos juntos
	// como pista, no como gate duro.
	if !looksLikeCSV(fh.Filename, fh.Header.Get("Content-Type")) {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("sube un archivo .csv exportado de Excel"))
		return
	}

	src, err := fh.Open()
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	defer src.Close()

	allowDup := strings.EqualFold(c.Query("allow_duplicates"), "true") ||
		strings.EqualFold(c.PostForm("allow_duplicates"), "true")

	out, err := ctrl.ImportCSV.Execute(c.Request.Context(), memApp.ImportMembersFromCSVInput{
		GymID:           gymID,
		ActorUserID:     userID,
		Reader:          src,
		AllowDuplicates: allowDup,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, importCSVResp{
		Imported: out.Imported,
		Skipped:  out.Skipped,
		Errors:   out.Errors,
		Summary: importCSVSummary{
			ImportedCount: out.ImportedCount,
			SkippedCount:  out.SkippedCount,
			ErrorsCount:   out.ErrorsCount,
			TotalDataRows: out.TotalDataRows,
			Atomic:        true,
		},
	})
}

// looksLikeCSV es una heurística amable: aceptamos cualquier filename con
// extensión .csv o un Content-Type que contenga "csv". No bloqueamos por
// MIME exacto porque Excel/Sheets/Numbers reportan inconsistentemente.
func looksLikeCSV(filename, contentType string) bool {
	if strings.HasSuffix(strings.ToLower(filename), ".csv") {
		return true
	}
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "csv")
}

// ---------------------------------------------------------------------------
// Wire mappers
// ---------------------------------------------------------------------------

func toMemberResp(m *memberDomain.Member) memberResp {
	if m == nil {
		return memberResp{}
	}
	r := memberResp{
		ID:                   m.ID,
		GymID:                m.GymID,
		Folio:                m.Folio,
		FullName:             m.FullName,
		Phone:                m.Phone,
		Email:                m.Email,
		PhotoURL:             m.PhotoURL,
		Notes:                m.Notes,
		Status:               m.Status,
		EnrollmentPaid:       m.EnrollmentPaid,
		HasPin:               m.PinHash != nil,
		LastContactAttemptAt: m.LastContactAttemptAt,
		Gender:               m.Gender,
		CreatedAt:            m.CreatedAt,
	}
	if m.PinPlain != nil {
		r.Pin = *m.PinPlain
	}
	if m.Birthdate != nil {
		s := m.Birthdate.Format("2006-01-02")
		r.Birthdate = &s
	}
	if m.LastMaintenancePaid != nil {
		s := m.LastMaintenancePaid.Format("2006-01-02")
		r.LastMaintenancePaid = &s
	}
	return r
}

func toMembershipResp(ms *membershipDomain.Membership) *membershipResp {
	if ms == nil {
		return nil
	}
	return &membershipResp{
		ID:                   ms.ID,
		MembershipTypeID:     ms.MembershipTypeID,
		TypeName:             ms.TypeNameSnapshot,
		Price:                ms.PriceSnapshot,
		StartDate:            ms.StartDate.Format("2006-01-02"),
		ExpiryDate:           formatOptionalDate(ms.ExpiryDate),
		Status:               ms.Status,
		DurationDaysSnapshot: ms.DurationDaysSnapshot,
	}
}

// formatOptionalDate renders a nullable date as YYYY-MM-DD, empty when nil.
// Used at the wire boundary for membership fields that may be unset while a
// membership is in pending_payment.
func formatOptionalDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// access enum re-export so callers compiling outside this package don't import members/domain/access.
var _ access.AccessStatus = access.AllowedActive
