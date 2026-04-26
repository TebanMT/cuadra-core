//go:build server

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	infraDB "github.com/cuadra/cuadra-core/infraestructure/db"

	gymApp "github.com/cuadra/cuadra-core/src/modules/gyms/app"
	gymRepoPg "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memRepoPg "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/repositories"
	memCtrl "github.com/cuadra/cuadra-core/src/modules/members/interfaces/controllers"

	chkApp "github.com/cuadra/cuadra-core/src/modules/checkins/app"
	chkRepoPg "github.com/cuadra/cuadra-core/src/modules/checkins/infraestructure/db/repositories"
	chkCtrl "github.com/cuadra/cuadra-core/src/modules/checkins/interfaces/controllers"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	billingRepoPg "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	billingCtrl "github.com/cuadra/cuadra-core/src/modules/billing/interfaces/controllers"

	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	prodRepoPg "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	prodCtrl "github.com/cuadra/cuadra-core/src/modules/products/interfaces/controllers"

	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoPg "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	usersCtrl "github.com/cuadra/cuadra-core/src/modules/users/interfaces/controllers"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain"
	notiRepoPg "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/repositories"
	notiEmail "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/email"
	notiWhatsApp "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/whatsapp"
	notiCtrl "github.com/cuadra/cuadra-core/src/modules/notifications/interfaces/controllers"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	reportsCtrl "github.com/cuadra/cuadra-core/src/application/reports/interfaces"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/email"
	syncShared "github.com/cuadra/cuadra-core/src/shared/sync"
)

func main() {
	_ = godotenv.Load()

	dsn := mustEnv("DATABASE_URL")
	db := infraDB.InitPostgres(dsn)
	defer infraDB.ClosePostgres()

	if err := infraDB.ApplyPostgresMigrations(db, envOrDefault("MIGRATIONS_DIR", "db_migrations/postgres")); err != nil {
		log.Fatalf("apply migrations: %v", err)
	}

	uow := sharedDomain.NewPostgresUnitOfWork(db)

	// ── Repositories ───────────────────────────────────────────────────────
	gymRepo := gymRepoPg.NewGymPostgresRepository()
	transferRepo := gymRepoPg.NewTransferPostgresRepository()
	otpRepo := gymRepoPg.NewTransferOTPPostgresRepository()
	userRepo := usersRepoPg.NewUserPostgresRepository()
	resetRepo := usersRepoPg.NewPasswordResetPostgresRepository()
	blRepo := usersRepoPg.NewRefreshTokenBlacklistPostgresRepository()
	mtRepo := memRepoPg.NewMembershipTypePostgresRepository()
	memberRepo := memRepoPg.NewMemberPostgresRepository()
	membershipRepo := memRepoPg.NewMembershipPostgresRepository()
	adjustmentRepo := memRepoPg.NewMembershipAdjustmentPostgresRepository()
	paymentRepo := billingRepoPg.NewPaymentPostgresRepository()
	saleRepo := billingRepoPg.NewSalePostgresRepository()
	saleItemRepo := billingRepoPg.NewSaleItemPostgresRepository()
	cashCloseReader := billingRepoPg.NewCashClosePostgresReader()
	cashCloseEventRepo := billingRepoPg.NewCashCloseEventPostgresRepository()
	productRepo := prodRepoPg.NewProductPostgresRepository()
	stockMovementRepo := prodRepoPg.NewStockMovementPostgresRepository()
	fingerprintRepo := memRepoPg.NewFingerprintPostgresRepository()
	checkinRepo := chkRepoPg.NewCheckinPostgresRepository()
	contactAttemptRepo := memRepoPg.NewContactAttemptPostgresRepository()
	reportsReader := reportsInfra.NewPostgresReader()
	notificationRepo := notiRepoPg.NewNotificationPostgresRepository()
	templateRepo := notiRepoPg.NewTemplateOverridePostgresRepository()
	whatsappEventRepo := notiRepoPg.NewWhatsAppEventPostgresRepository()
	expiryReader := notiRepoPg.NewExpiryPostgresReader()

	// ── Shared services ────────────────────────────────────────────────────
	tokens := auth.NewJWTService(mustEnv("JWT_SECRET"))
	recorder := audit.NewPostgresRecorder()
	emailSender := email.NewStdoutSender()
	trialDays := envInt("TRIAL_DURATION_DAYS", 30)
	baseURL := envOrDefault("PUBLIC_BASE_URL", "https://cuadra.app")

	// ── Notifications providers (Sesión 7 / ADR-007) ─────────────────────
	whatsappProvider := buildWhatsAppProvider(baseURL)
	notiEmailProvider := buildEmailProvider()

	// ── Use cases ──────────────────────────────────────────────────────────
	signup := usersApp.NewSignupOwner(userRepo, gymRepo, uow, tokens, recorder, trialDays)
	login := usersApp.NewLogin(userRepo, gymRepo, uow, tokens, recorder)
	logout := usersApp.NewLogout(blRepo, uow, tokens, recorder)
	requestReset := usersApp.NewRequestPasswordReset(userRepo, resetRepo, uow, emailSender, recorder, baseURL)
	confirmReset := usersApp.NewConfirmPasswordReset(userRepo, resetRepo, blRepo, uow, recorder)
	updateBasic := gymApp.NewUpdateBasicInfo(gymRepo, uow, recorder)
	updatePay := gymApp.NewUpdatePaymentMethods(gymRepo, uow, recorder)
	completeSetup := gymApp.NewCompleteSetup(gymRepo, uow, recorder)
	updateProfile := gymApp.NewUpdateProfile(gymRepo, uow, recorder)
	createOp := usersApp.NewCreateOperator(userRepo, uow, recorder)
	updateOp := usersApp.NewUpdateOperator(userRepo, uow, recorder)
	toggleOp := usersApp.NewToggleOperatorActive(userRepo, blRepo, uow, recorder)
	resetOp := usersApp.NewResetOperatorPassword(userRepo, blRepo, uow, recorder)
	requestTransfer := usersApp.NewRequestTransferOwnership(userRepo, otpRepo, uow, recorder, emailSender)
	confirmTransfer := usersApp.NewConfirmTransferOwnership(userRepo, otpRepo, transferRepo, blRepo, uow, recorder, emailSender)
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

	// ── Biometric (UC-028, UC-029, UC-032) ────────────────────────────────
	// Cloud uses an in-memory GMK provider seeded from the GYM_DEMO_GMK_SEED
	// env var when present — production cloud doesn't decrypt templates
	// (kiosko keeps them encrypted), so the provider is mostly here for
	// integration tests / dashboard read-only flows.
	gmkProvider := bcrypto.NewInMemoryGMKProvider()
	registerFingerprint := memApp.NewRegisterFingerprint(memberRepo, fingerprintRepo, gmkProvider, uow, recorder)
	checkinManual := chkApp.NewCheckinManual(memberSvc, checkinRepo, uow, recorder)
	checkinPin := chkApp.NewCheckinByPin(memberSvc, memberRepo, checkinRepo, uow, recorder, nil)
	checkinOverride := chkApp.NewOverrideCheckin(memberSvc, checkinRepo, uow, recorder)

	// ── Notifications (Sesión 7) ──────────────────────────────────────────
	enqueueReceipt := notiApp.NewEnqueueReceipt(notificationRepo, gymRepo, memberRepo, uow)
	enqueueExpiry := notiApp.NewEnqueueExpiryReminder(notificationRepo, expiryReader, uow)
	enqueueOwnerAlert := notiApp.NewEnqueueOwnerAlert(notificationRepo, gymRepo, userRepo, uow)
	dispatchNoti := notiApp.NewDispatchNotification(notificationRepo, templateRepo, gymRepo, whatsappProvider, notiEmailProvider, uow)
	connectWhatsApp := notiApp.NewConnectWhatsApp(gymRepo, whatsappProvider, uow, recorder)
	whatsappStatus := notiApp.NewGetWhatsAppStatus(gymRepo, uow)
	listTemplates := notiApp.NewListTemplates(templateRepo, uow)
	updateTemplate := notiApp.NewUpdateTemplate(templateRepo, uow, recorder)
	broadcast := notiApp.NewBroadcast(notificationRepo, memberRepo, gymRepo, uow, recorder)
	listNotifications := notiApp.NewListNotifications(notificationRepo, uow)
	processWebhook := notiApp.NewProcessWebhook(notificationRepo, whatsappEventRepo, uow)
	billingSubscriber := notiApp.NewBillingEventSubscriber(enqueueReceipt)

	// ── Billing (Sesión 3) ────────────────────────────────────────────────
	folios := folioSvc.NewGenerator(paymentRepo)
	registerPayment := billingApp.NewRegisterMembershipPayment(paymentRepo, folios, memberSvc, memberRepo, uow, recorder, billingSubscriber)
	settlePayment := billingApp.NewSettlePendingBalance(paymentRepo, folios, uow, recorder)
	receiptPayment := billingApp.NewGenerateReceipt(paymentRepo, gymRepo, memberRepo, uow)
	sendReceipt := billingApp.NewSendReceipt(paymentRepo, uow)
	listMemberPayments := billingApp.NewListMemberPayments(paymentRepo, memberRepo, uow)
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
	cashClose := reportsApp.NewCashClose(cashCloseReader, cashCloseEventRepo, uow, recorder)

	// ── Reports application layer (Sesión 6) ─────────────────────────────
	dashboard := reportsApp.NewDashboard(reportsReader, uow, 60*time.Second)
	attentionRequired := reportsApp.NewAttentionRequired(reportsReader, uow)
	exportReport := reportsApp.NewExportReport(reportsReader, gymRepo, uow, attentionRequired)
	markContacted := memApp.NewMarkContacted(memberRepo, contactAttemptRepo, uow, recorder)
	markLost := memApp.NewMarkLost(memberRepo, uow, recorder)

	// ── Controllers ────────────────────────────────────────────────────────
	authCtrl := usersCtrl.NewAuthController(usersCtrl.AuthController{
		Signup:           signup,
		Login:            login,
		Logout:           logout,
		RequestReset:     requestReset,
		ConfirmReset:     confirmReset,
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
	})
	mtCtrl := memCtrl.NewMembershipTypeController(createMT, updateMT, deactivateMT, listMT, tokens)
	memberCtrl := memCtrl.NewMemberController(createMember, updateMember, listMembers, memberDetail, toggleMember, lockExpiry, assignPin, tokens)
	fingerprintCtrl := memCtrl.NewFingerprintController(registerFingerprint, tokens)
	paymentCtrl := billingCtrl.NewPaymentController(registerPayment, settlePayment, receiptPayment, sendReceipt, listMemberPayments, refundPayment, registerSale, refundSale, cashClose, tokens)
	productCtrl := prodCtrl.NewProductController(createProduct, updateProduct, deactivateProduct, listProducts, adjustStock, tokens)
	checkinCtrl := chkCtrl.NewCheckinController(checkinManual, checkinPin, checkinOverride, tokens)
	reportsController := reportsCtrl.NewReportsController(dashboard, attentionRequired, exportReport, markContacted, markLost, tokens)
	notificationsCtrl := notiCtrl.NewController(connectWhatsApp, whatsappStatus, listTemplates, updateTemplate, broadcast, listNotifications, tokens)
	twilioWebhookURL := envOrDefault("TWILIO_WEBHOOK_URL", baseURL+"/api/v1/webhooks/twilio")
	notiWebhookCtrl := notiCtrl.NewWebhookController(processWebhook, envOrDefault("TWILIO_AUTH_TOKEN", ""), twilioWebhookURL)
	_ = enqueueOwnerAlert // wired into future hooks; kept resolved so build doesn't drop it.

	// ── Gin router ────────────────────────────────────────────────────────
	if os.Getenv("ENVIRONMENT") == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.Default()
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "cuadra-server"})
	})
	authCtrl.RegisterRoutes(r)
	mtCtrl.RegisterRoutes(r)
	memberCtrl.RegisterRoutes(r)
	fingerprintCtrl.RegisterRoutes(r)
	paymentCtrl.RegisterRoutes(r)
	productCtrl.RegisterRoutes(r)
	checkinCtrl.RegisterRoutes(r)
	reportsController.RegisterRoutes(r)
	notificationsCtrl.RegisterRoutes(r)
	notiWebhookCtrl.RegisterRoutes(r)

	// Sync protocol (Sesión 8 / ADR-001) — push/pull/full + Prometheus
	// metrics at /_internal/metrics. The handler depends only on the UoW
	// already wired above; Store and ConflictLogger are stateless.
	syncMetrics := syncShared.NewMetrics()
	syncStore := syncShared.NewPostgresStore()
	syncConflicts := syncShared.NewConflictLogger()
	syncHandler := syncShared.NewHandler(uow, syncStore, syncConflicts, tokens, syncMetrics)
	syncHandler.RegisterRoutes(r)

	// Background workers (Sesión 7 §dispatcher + scheduler).
	bgCtx, cancelBg := context.WithCancel(context.Background())
	defer cancelBg()
	dispatchInterval := time.Duration(envInt("NOTIFICATIONS_DISPATCH_INTERVAL_S", 5)) * time.Second
	expiryInterval := time.Duration(envInt("NOTIFICATIONS_EXPIRY_INTERVAL_M", 60)) * time.Minute
	dispatchWorker := notiApp.NewWorker(dispatchNoti, dispatchInterval)
	expiryScheduler := notiApp.NewScheduler(enqueueExpiry, expiryInterval)
	go dispatchWorker.Start(bgCtx)
	go expiryScheduler.Start(bgCtx)

	port := envOrDefault("PORT", "8080")
	log.Printf("cuadra-server starting on :%s", port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf("gin: %v", err)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s not set", key)
	}
	return v
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

// buildWhatsAppProvider wires the configured WhatsApp provider. When
// `WHATSAPP_PROVIDER=twilio` and creds are present, the Twilio impl is
// used; otherwise we fall back to stdout (logs only, dev-friendly).
func buildWhatsAppProvider(baseURL string) notiDomain.WhatsAppProvider {
	provider := strings.ToLower(envOrDefault("WHATSAPP_PROVIDER", "stdout"))
	if provider == "twilio" {
		sid := os.Getenv("TWILIO_ACCOUNT_SID")
		token := os.Getenv("TWILIO_AUTH_TOKEN")
		if sid == "" || token == "" {
			log.Printf("[notifications] TWILIO creds missing — falling back to stdout provider")
			return notiWhatsApp.NewStdoutProvider()
		}
		opts := notiWhatsApp.TwilioOptions{
			AccountSID:        sid,
			AuthToken:         token,
			StatusCallbackURL: envOrDefault("TWILIO_WEBHOOK_URL", baseURL+"/api/v1/webhooks/twilio"),
		}
		p, err := notiWhatsApp.NewTwilioProvider(opts)
		if err != nil {
			log.Printf("[notifications] twilio init failed: %v — using stdout fallback", err)
			return notiWhatsApp.NewStdoutProvider()
		}
		return p
	}
	if provider == "mock" {
		return notiWhatsApp.NewMockProvider()
	}
	return notiWhatsApp.NewStdoutProvider()
}

// buildEmailProvider wires the configured email provider for cross-BC use
// (notifications-side EmailProvider, separate from the legacy
// shared/email.Sender used by users/auth flows).
func buildEmailProvider() notiDomain.EmailProvider {
	provider := strings.ToLower(envOrDefault("EMAIL_PROVIDER", "stdout"))
	if provider == "resend" {
		key := os.Getenv("RESEND_API_KEY")
		from := envOrDefault("EMAIL_FROM", "noreply@cuadra.app")
		if key == "" {
			log.Printf("[notifications] RESEND_API_KEY missing — falling back to stdout email provider")
			return notiEmail.NewStdoutProvider()
		}
		p, err := notiEmail.NewResendProvider(notiEmail.ResendOptions{APIKey: key, From: from})
		if err != nil {
			log.Printf("[notifications] resend init failed: %v — using stdout fallback", err)
			return notiEmail.NewStdoutProvider()
		}
		return p
	}
	if provider == "mock" {
		return notiEmail.NewMockProvider()
	}
	return notiEmail.NewStdoutProvider()
}
