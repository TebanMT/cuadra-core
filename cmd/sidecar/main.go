//go:build sidecar

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"

	migrations "github.com/cuadra/cuadra-core/db_migrations"
	infraDB "github.com/cuadra/cuadra-core/infraestructure/db"

	gymApp "github.com/cuadra/cuadra-core/src/modules/gyms/app"
	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memRepoLite "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/repositories"
	memCtrl "github.com/cuadra/cuadra-core/src/modules/members/interfaces/controllers"

	chkApp "github.com/cuadra/cuadra-core/src/modules/checkins/app"
	chkRepoLite "github.com/cuadra/cuadra-core/src/modules/checkins/infraestructure/db/repositories"
	chkCtrl "github.com/cuadra/cuadra-core/src/modules/checkins/interfaces/controllers"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	billingRepoLite "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	billingCtrl "github.com/cuadra/cuadra-core/src/modules/billing/interfaces/controllers"

	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	prodRepoLite "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	prodCtrl "github.com/cuadra/cuadra-core/src/modules/products/interfaces/controllers"

	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	usersCtrl "github.com/cuadra/cuadra-core/src/modules/users/interfaces/controllers"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notiRepoLite "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/repositories"
	notiEmail "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/email"
	notiWhatsApp "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/whatsapp"
	notiCtrl "github.com/cuadra/cuadra-core/src/modules/notifications/interfaces/controllers"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	reportsCtrl "github.com/cuadra/cuadra-core/src/application/reports/interfaces"
	"github.com/cuadra/cuadra-core/src/shared/accesswebhook"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/email"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	syncShared "github.com/cuadra/cuadra-core/src/shared/sync"
)

func main() {
	_ = godotenv.Load()

	dbPath := envOrDefault("SIDECAR_DB_PATH", "./tmp/cuadra.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}
	db := infraDB.InitSQLite(dbPath)
	defer infraDB.CloseSQLite()

	// Migrations are embedded into the binary at build time (see
	// db_migrations/embed.go) so the sidecar runs from any cwd —
	// including inside a macOS .app bundle, where the working
	// directory is "/" and relative paths break.
	if err := infraDB.ApplySQLiteMigrations(db, migrations.SQLite, "sqlite"); err != nil {
		log.Fatalf("apply sqlite migrations: %v", err)
	}

	queue := syncShared.NewSqliteQueue()
	uow := sharedDomain.NewSQLiteUnitOfWork(db, queue)

	// ── Repositories ───────────────────────────────────────────────────────
	gymRepo := gymRepoLite.NewGymSQLiteRepository()
	transferRepo := gymRepoLite.NewTransferSQLiteRepository()
	otpRepo := gymRepoLite.NewTransferOTPSQLiteRepository()
	userRepo := usersRepoLite.NewUserSQLiteRepository()
	mtRepo := memRepoLite.NewMembershipTypeSQLiteRepository()
	memberRepo := memRepoLite.NewMemberSQLiteRepository()
	membershipRepo := memRepoLite.NewMembershipSQLiteRepository()
	adjustmentRepo := memRepoLite.NewMembershipAdjustmentSQLiteRepository()
	paymentRepo := billingRepoLite.NewPaymentSQLiteRepository()
	saleRepo := billingRepoLite.NewSaleSQLiteRepository()
	saleItemRepo := billingRepoLite.NewSaleItemSQLiteRepository()
	cashCloseReader := billingRepoLite.NewCashCloseSQLiteReader()
	cashCloseEventRepo := billingRepoLite.NewCashCloseEventSQLiteRepository()
	productRepo := prodRepoLite.NewProductSQLiteRepository()
	stockMovementRepo := prodRepoLite.NewStockMovementSQLiteRepository()
	fingerprintRepo := memRepoLite.NewFingerprintSQLiteRepository()
	checkinRepo := chkRepoLite.NewCheckinSQLiteRepository()
	contactAttemptRepo := memRepoLite.NewContactAttemptSQLiteRepository()
	reportsReader := reportsInfra.NewSQLiteReader()
	notificationRepo := notiRepoLite.NewNotificationSQLiteRepository()
	templateRepo := notiRepoLite.NewTemplateOverrideSQLiteRepository()
	alertConfigRepo := notiRepoLite.NewAlertConfigSQLiteRepository()

	// ── Shared services ────────────────────────────────────────────────────
	// Sidecar JWTs are issued for the on-premise reception screen — there
	// is no shared device and no realistic threat model that justifies
	// kicking the operator off mid-shift, so we mint effectively-eternal
	// tokens (≈100 years). The desktop's only logout is the explicit
	// "Cerrar sesión" action; everything else (refresh, restart, sleep)
	// keeps the session alive. Override via env if you ever need otherwise.
	accessTTL := time.Duration(envInt("ACCESS_TOKEN_TTL_HOURS", 24*365*100)) * time.Hour
	refreshTTL := time.Duration(envInt("REFRESH_TOKEN_TTL_HOURS", 24*365*100)) * time.Hour
	tokens := auth.NewJWTServiceWithDurations(
		envOrDefault("JWT_SECRET", "sidecar-dev-secret-do-not-use-in-prod"),
		accessTTL, refreshTTL,
	)
	recorder := audit.NewSQLiteRecorder()
	emailSender := email.NewStdoutSender()
	trialDays := envInt("TRIAL_DURATION_DAYS", 30)

	// Biometric reader (UC-028, UC-029, UC-031). The actual implementation is
	// chosen by build tag (digitalpersona.go for `bio_dp`, mock.go for
	// `bio_mock`, digitalpersona_disabled.go for the default dev sidecar).
	bioReader := biometric.NewDigitalPersonaReader()
	gmkProvider := bcrypto.NewInMemoryGMKProvider()
	// TODO(humano — Sesión 8): swap InMemoryGMKProvider for an OS-keychain
	// provider that reads cuadra.gmk.<gym_id> via Tauri command. The current
	// in-memory provider is dev-only and forgets keys on restart.

	// Sidecar use cases — password-reset / refresh-blacklist flows (UC-003,
	// UC-004, UC-008/9 token revocation) live cloud-side, so the sidecar
	// passes nil for those and the relevant routes return UnsupportedOffline.
	signup := usersApp.NewSignupOwner(userRepo, gymRepo, uow, tokens, recorder, trialDays)
	login := usersApp.NewLogin(userRepo, gymRepo, uow, tokens, recorder)
	logout := usersApp.NewLogout(nil, uow, tokens, recorder)
	updateBasic := gymApp.NewUpdateBasicInfo(gymRepo, uow, recorder)
	updatePay := gymApp.NewUpdatePaymentMethods(gymRepo, uow, recorder)
	completeSetup := gymApp.NewCompleteSetup(gymRepo, uow, recorder)
	updateProfile := gymApp.NewUpdateProfile(gymRepo, uow, recorder)
	createOp := usersApp.NewCreateOperator(userRepo, uow, recorder)
	updateOp := usersApp.NewUpdateOperator(userRepo, uow, recorder)
	toggleOp := usersApp.NewToggleOperatorActive(userRepo, nil, uow, recorder)
	resetOp := usersApp.NewResetOperatorPassword(userRepo, nil, uow, recorder)
	requestTransfer := usersApp.NewRequestTransferOwnership(userRepo, otpRepo, uow, recorder, emailSender)
	confirmTransfer := usersApp.NewConfirmTransferOwnership(userRepo, otpRepo, transferRepo, nil, uow, recorder, emailSender)
	createMT := memApp.NewCreateMembershipType(mtRepo, uow, recorder)
	updateMT := memApp.NewUpdateMembershipType(mtRepo, uow, recorder)
	deactivateMT := memApp.NewDeactivateMembershipType(mtRepo, uow, recorder)
	listMT := memApp.NewListMembershipTypes(mtRepo, uow)
	createMember := memApp.NewCreateMember(memberRepo, membershipRepo, mtRepo, uow, recorder)
	updateMember := memApp.NewUpdateMember(memberRepo, uow, recorder)
	listMembers := memApp.NewListMembers(memberRepo, uow)
	memberDetail := memApp.NewGetMemberDetail(memberRepo, uow)
	toggleMember := memApp.NewToggleMemberStatus(memberRepo, uow, recorder)
	lockExpiry := memApp.NewLockMembershipExpiry(membershipRepo, adjustmentRepo, uow, recorder)
	assignPin := memApp.NewAssignPin(memberRepo, uow, recorder)
	memberSvc := memApp.NewMemberService(memberRepo, membershipRepo, mtRepo).WithFingerprints(fingerprintRepo)

	// ── Biometric + Checkins (Sesión 5) ───────────────────────────────────
	registerFingerprint := memApp.NewRegisterFingerprint(memberRepo, fingerprintRepo, gmkProvider, uow, recorder)
	checkinManual := chkApp.NewCheckinManual(memberSvc, checkinRepo, uow, recorder)
	checkinPin := chkApp.NewCheckinByPin(memberSvc, memberRepo, checkinRepo, uow, recorder, nil)
	checkinOverride := chkApp.NewOverrideCheckin(memberSvc, checkinRepo, uow, recorder)
	checkinFingerprint := chkApp.NewCheckinByFingerprint(memberSvc, checkinRepo, bioReader, uow, recorder)
	kioskEvents := chkApp.NewKioskBroadcaster()
	// kioskGymID is left zero until the operator logs in — the kiosko start
	// endpoint sets it from the auth context. For now we wire a placeholder
	// loop with uuid.Nil; Start() will be called by the controller with the
	// real gym ID. (TODO Sesión 6: bind GymID at Start time.)
	kioskLoop := chkApp.NewKioskLoop(uuid.Nil, bioReader, checkinFingerprint, kioskEvents)

	// ── Notifications (Sesión 7) — sidecar enqueues only; cloud worker
	// drains the synced rows. The mock WhatsApp provider keeps connect
	// flows and tests usable offline.
	whatsappMock := notiWhatsApp.NewStdoutProvider()
	emailMock := notiEmail.NewStdoutProvider()
	enqueueReceipt := notiApp.NewEnqueueReceipt(notificationRepo, gymRepo, memberRepo, uow)
	enqueueOwnerAlert := notiApp.NewEnqueueOwnerAlert(notificationRepo, gymRepo, userRepo, alertConfigRepo, uow)
	connectWhatsApp := notiApp.NewConnectWhatsApp(gymRepo, whatsappMock, uow, recorder)
	whatsappStatus := notiApp.NewGetWhatsAppStatus(gymRepo, uow)
	listTemplates := notiApp.NewListTemplates(templateRepo, uow)
	updateTemplate := notiApp.NewUpdateTemplate(templateRepo, uow, recorder)
	listOwnerAlerts := notiApp.NewListOwnerAlerts(alertConfigRepo, uow)
	updateOwnerAlert := notiApp.NewUpdateOwnerAlert(alertConfigRepo, uow, recorder)
	broadcast := notiApp.NewBroadcast(notificationRepo, memberRepo, gymRepo, uow, recorder)
	listNotifications := notiApp.NewListNotifications(notificationRepo, uow)
	billingSubscriber := notiApp.NewBillingEventSubscriber(enqueueReceipt)
	_ = emailMock

	// ── Billing (Sesión 3) ────────────────────────────────────────────────
	folios := folioSvc.NewGenerator(paymentRepo)
	registerPayment := billingApp.NewRegisterMembershipPayment(paymentRepo, folios, memberSvc, memberRepo, uow, recorder, billingSubscriber)
	settlePayment := billingApp.NewSettlePendingBalance(paymentRepo, folios, uow, recorder)
	receiptPayment := billingApp.NewGenerateReceipt(paymentRepo, gymRepo, memberRepo, uow)
	sendReceipt := billingApp.NewSendReceipt(paymentRepo, uow)
	listMemberPayments := billingApp.NewListMemberPayments(paymentRepo, memberRepo, uow)
	listGymPayments := billingApp.NewListGymPayments(paymentRepo, memberRepo, uow)
	refundPayment := billingApp.NewRefundPayment(paymentRepo, folios, memberSvc, uow, recorder)

	// ── Products + Billing pt.2 (Sesión 4) ────────────────────────────────
	productSvc := prodApp.NewProductService(productRepo, stockMovementRepo)
	createProduct := prodApp.NewCreateProduct(productRepo, stockMovementRepo, uow, recorder)
	updateProduct := prodApp.NewUpdateProduct(productRepo, uow, recorder)
	deactivateProduct := prodApp.NewDeactivateProduct(productRepo, uow, recorder)
	listProducts := prodApp.NewListProducts(productRepo, uow)
	adjustStock := prodApp.NewAdjustStock(productRepo, stockMovementRepo, uow, recorder)
	registerSale := billingApp.NewRegisterSale(paymentRepo, saleRepo, saleItemRepo, folios, productSvc, memberRepo, uow, recorder, billingSubscriber)
	refundSale := billingApp.NewRefundSale(saleRepo, refundPayment, uow)
	cashClose := reportsApp.NewCashClose(cashCloseReader, cashCloseEventRepo, uow, recorder).
		WithSubscriber(notiApp.NewCashCloseAlertSubscriber(enqueueOwnerAlert))

	// ── Reports application layer (Sesión 6) — same use cases as the cloud,
	// but reading from the local SQLite. The dashboard cache TTL matches the
	// server build (60s) so the FE behaves identically online/offline.
	dashboard := reportsApp.NewDashboard(reportsReader, uow, 60*time.Second)
	attentionRequired := reportsApp.NewAttentionRequired(reportsReader, uow)
	rangeReport := reportsApp.NewRangeReport(reportsReader, uow)
	exportReport := reportsApp.NewExportReport(reportsReader, gymRepo, uow, attentionRequired)
	markContacted := memApp.NewMarkContacted(memberRepo, contactAttemptRepo, uow, recorder)
	markLost := memApp.NewMarkLost(memberRepo, uow, recorder)

	authCtrl := usersCtrl.NewAuthController(usersCtrl.AuthController{
		Signup:           signup,
		Login:            login,
		Logout:           logout,
		UpdateBasicInfo:  updateBasic,
		UpdatePayMethods: updatePay,
		CompleteSetup:    completeSetup,
		UpdateProfile:    updateProfile,
		CreateOperator:   createOp,
		UpdateOperator:   updateOp,
		ToggleActive:     toggleOp,
		ResetOpPassword:  resetOp,
		RequestTransfer:  requestTransfer,
		ConfirmTransfer:  confirmTransfer,
		Tokens:           tokens,
		Gyms:             gymRepo,
		Users:            userRepo,
		MembershipTypes:  mtRepo,
		UoW:              uow,
		UploadsDir:       envOrDefault("UPLOADS_DIR", "./tmp/uploads"),
	})
	mtCtrl := memCtrl.NewMembershipTypeController(createMT, updateMT, deactivateMT, listMT, tokens)
	memberCtrl := memCtrl.NewMemberController(createMember, updateMember, listMembers, memberDetail, toggleMember, lockExpiry, assignPin, tokens)
	fingerprintCtrl := memCtrl.NewFingerprintController(registerFingerprint, tokens)
	paymentCtrl := billingCtrl.NewPaymentController(registerPayment, settlePayment, receiptPayment, sendReceipt, listMemberPayments, listGymPayments, refundPayment, registerSale, refundSale, cashClose, tokens)
	productCtrl := prodCtrl.NewProductController(createProduct, updateProduct, deactivateProduct, listProducts, adjustStock, tokens)
	fingerprintAvailable := func() bool { return bioReader.Available(context.Background()) }
	// Outbound access-granted webhook (Fase 1 differentiator: lets the gym
	// drive any turnstile / cerradura over HTTP). URL + HMAC secret live in
	// gyms.kiosk_settings; dispatcher reads them per-call.
	accessWebhook := accesswebhook.NewHTTPDispatcher(uow, gymRepo)
	checkinCtrl := chkCtrl.NewCheckinController(checkinManual, checkinPin, checkinOverride, checkinRepo, uow, fingerprintAvailable, tokens).
		WithWebhook(accessWebhook)
	kioskCtrl := chkCtrl.NewKioskController(checkinFingerprint, kioskLoop, kioskEvents, bioReader, tokens).
		WithSibling(checkinCtrl)
	notificationsCtrl := notiCtrl.NewController(connectWhatsApp, whatsappStatus, listTemplates, updateTemplate, broadcast, listNotifications, listOwnerAlerts, updateOwnerAlert, whatsappMock, tokens)
	reportsController := reportsCtrl.NewReportsController(dashboard, attentionRequired, rangeReport, exportReport, markContacted, markLost, tokens)

	if os.Getenv("ENVIRONMENT") == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.Default()
	r.Use(middleware.CORS(middleware.CORSConfig{
		AllowedOrigins: parseOrigins(envOrDefault("CORS_ALLOWED_ORIGINS", "http://localhost:5173,tauri://localhost,http://tauri.localhost")),
	}))
	r.Use(localTokenMiddleware(envOrDefault("LOCAL_AUTH_TOKEN", "")))
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "cuadra-sidecar"})
	})
	// Auth routing on the sidecar:
	//   /api/v1/auth/{signup,login,refresh,logout,forgot-password,reset-password,
	//                  verify-password,redeem-installer}  → SidecarAuthProxy
	//                                                        (forwards to cloud
	//                                                        + persists sidecar
	//                                                        token + caches
	//                                                        offline credentials
	//                                                        + resigns JWTs)
	//   /api/v1/auth/me                                    → local AuthController
	//   /api/v1/users/*                                    → local AuthController
	//   /api/v1/gyms/me/*                                  → local AuthController
	//
	// The cloud's signup/login response includes a sidecar_token that the
	// proxy stores in sync_state; the agent picks it up automatically and
	// stops depending on the operator's JWT.
	clientID, deviceLabel := mustEnsureSidecarClientID(uow)
	cloudURL := envOrDefault("TINTA_CLOUD_URL", "https://api.entinta.app")
	authProxy := usersCtrl.NewSidecarAuthProxy(usersCtrl.SidecarAuthProxy{
		CloudURL:    cloudURL,
		UoW:         uow,
		LocalTokens: tokens,
		ClientID:    clientID,
		DeviceLabel: deviceLabel,
	})
	authProxy.RegisterRoutes(r)
	authCtrl.RegisterMeRoute(r)
	authCtrl.RegisterAccountRoutes(r)
	authCtrl.RegisterOperatorRoutes(r)
	authCtrl.RegisterUploadsRoute(r)
	mtCtrl.RegisterRoutes(r)
	memberCtrl.RegisterRoutes(r)
	fingerprintCtrl.RegisterRoutes(r)
	paymentCtrl.RegisterRoutes(r)
	productCtrl.RegisterRoutes(r)
	checkinCtrl.RegisterRoutes(r)
	kioskCtrl.RegisterRoutes(r)
	notificationsCtrl.RegisterRoutes(r)
	reportsController.RegisterRoutes(r)

	// ── Sync agent (Sesión 8 / ADR-001) ──────────────────────────────────
	// The agent reads its sk_live_* sidecar credential from sync_state on
	// boot — the SidecarAuthProxy persisted it during the operator's first
	// online login (ADR-008 §3.3). The agent never sees an operator JWT.
	syncInterval := time.Duration(envInt("SYNC_INTERVAL_S", 30)) * time.Second
	agent := syncShared.NewAgent(syncShared.AgentConfig{
		BaseURL:  cloudURL,
		Interval: syncInterval,
		Logger:   log.New(os.Stderr, "[sync] ", log.LstdFlags),
	}, db, uow)
	// Wire the proxy's reload callback so a fresh login bumps the agent
	// without waiting for the next tick.
	authProxy.AgentReload = agent.TriggerNow
	syncStatusCtrl := syncShared.NewStatusController(agent)
	syncStatusCtrl.RegisterRoutes(r)

	syncCtx, cancelSync := context.WithCancel(context.Background())
	defer cancelSync()
	go agent.Run(syncCtx)

	port := envOrDefault("SIDECAR_PORT", "9090")
	// ADR-003 §2.2: print the port to stdout so Tauri can capture it.
	fmt.Printf("LISTENING_ON=%s\n", port)
	log.Printf("tinta-sidecar starting on 127.0.0.1:%s db=%s", port, dbPath)
	if err := r.Run("127.0.0.1:" + port); err != nil {
		log.Fatalf("gin: %v", err)
	}
}

// mustEnsureSidecarClientID materialises the per-installation UUID from
// sync_state (creating it if missing) and resolves a human-readable label
// for the dashboard's "active devices" panel.
func mustEnsureSidecarClientID(uow sharedDomain.UnitOfWork) (uuid.UUID, string) {
	var id uuid.UUID
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		got, err := syncShared.EnsureClientID(context.Background(), tx)
		if err != nil {
			return err
		}
		id = got
		return nil
	}); err != nil {
		log.Fatalf("ensure client_id: %v", err)
	}
	label, err := os.Hostname()
	if err != nil || label == "" {
		label = "tinta-sidecar"
	}
	if v := os.Getenv("TINTA_DEVICE_LABEL"); v != "" {
		label = v
	}
	return id, label
}

// localTokenMiddleware enforces the X-Local-Token header (ADR-003 §2.3) when
// LOCAL_AUTH_TOKEN is set. Empty token = open binding (dev mode).
func localTokenMiddleware(expected string) gin.HandlerFunc {
	if expected == "" {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		if c.GetHeader("X-Local-Token") != expected {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}

func parseOrigins(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
