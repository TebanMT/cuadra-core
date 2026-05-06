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

	subApp "github.com/cuadra/cuadra-core/src/modules/subscriptions/app"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	subDB "github.com/cuadra/cuadra-core/src/modules/subscriptions/infraestructure/db"
	subPay "github.com/cuadra/cuadra-core/src/modules/subscriptions/infraestructure/payments"
	subCtrl "github.com/cuadra/cuadra-core/src/modules/subscriptions/interfaces/controllers"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	reportsCtrl "github.com/cuadra/cuadra-core/src/application/reports/interfaces"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/email"
	"github.com/cuadra/cuadra-core/src/shared/installerbootstrap"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/sidecartoken"
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
	alertConfigRepo := notiRepoPg.NewAlertConfigPostgresRepository()
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
	sidecarStore := sidecartoken.NewPostgresStore(db)
	installerStore := installerbootstrap.NewPostgresStore(db)
	sidecarBootstrap := usersApp.NewBootstrapSidecarToken(sidecarStore, uow)
	issueInstaller := usersApp.NewIssueInstallerBootstrap(installerStore)
	redeemInstaller := usersApp.NewRedeemInstallerBootstrap(installerStore, userRepo, gymRepo, uow, tokens, sidecarBootstrap)

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
	enqueueOwnerAlert := notiApp.NewEnqueueOwnerAlert(notificationRepo, gymRepo, userRepo, alertConfigRepo, uow)
	dispatchNoti := notiApp.NewDispatchNotification(notificationRepo, templateRepo, gymRepo, whatsappProvider, notiEmailProvider, uow)
	connectWhatsApp := notiApp.NewConnectWhatsApp(gymRepo, whatsappProvider, uow, recorder)
	whatsappStatus := notiApp.NewGetWhatsAppStatus(gymRepo, uow)
	listTemplates := notiApp.NewListTemplates(templateRepo, uow)
	updateTemplate := notiApp.NewUpdateTemplate(templateRepo, uow, recorder)
	listOwnerAlerts := notiApp.NewListOwnerAlerts(alertConfigRepo, uow)
	updateOwnerAlert := notiApp.NewUpdateOwnerAlert(alertConfigRepo, uow, recorder)
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

	// ── Reports application layer (Sesión 6) ─────────────────────────────
	dashboard := reportsApp.NewDashboard(reportsReader, uow, 60*time.Second)
	attentionRequired := reportsApp.NewAttentionRequired(reportsReader, uow)
	rangeReport := reportsApp.NewRangeReport(reportsReader, uow)
	exportReport := reportsApp.NewExportReport(reportsReader, gymRepo, uow, attentionRequired)
	markContacted := memApp.NewMarkContacted(memberRepo, contactAttemptRepo, uow, recorder)
	markLost := memApp.NewMarkLost(memberRepo, uow, recorder)

	// ── Controllers ────────────────────────────────────────────────────────
	authCtrl := usersCtrl.NewAuthController(usersCtrl.AuthController{
		Signup:             signup,
		Login:              login,
		Logout:             logout,
		RequestReset:       requestReset,
		ConfirmReset:       confirmReset,
		UpdateBasicInfo:    updateBasic,
		UpdatePayMethods:   updatePay,
		CompleteSetup:      completeSetup,
		UpdateProfile:      updateProfile,
		CreateOperator:     createOp,
		UpdateOperator:     updateOp,
		ToggleActive:       toggleOp,
		ResetOpPassword:    resetOp,
		RequestTransfer:    requestTransfer,
		ConfirmTransfer:    confirmTransfer,
		Tokens:             tokens,
		Gyms:               gymRepo,
		Users:              userRepo,
		MembershipTypes:    mtRepo,
		UoW:                uow,
		UploadsDir:         envOrDefault("UPLOADS_DIR", "./tmp/uploads"),
		SidecarBootstrap:   sidecarBootstrap,
		InstallerBootstrap: issueInstaller,
		RedeemInstaller:    redeemInstaller,
	})
	mtCtrl := memCtrl.NewMembershipTypeController(createMT, updateMT, deactivateMT, listMT, tokens)
	memberCtrl := memCtrl.NewMemberController(createMember, updateMember, listMembers, memberDetail, toggleMember, lockExpiry, assignPin, tokens)
	fingerprintCtrl := memCtrl.NewFingerprintController(registerFingerprint, tokens)
	paymentCtrl := billingCtrl.NewPaymentController(registerPayment, settlePayment, receiptPayment, sendReceipt, listMemberPayments, listGymPayments, refundPayment, registerSale, refundSale, cashClose, tokens)
	productCtrl := prodCtrl.NewProductController(createProduct, updateProduct, deactivateProduct, listProducts, adjustStock, tokens)
	// Cloud has no biometric reader — fingerprint flows live on the sidecar.
	checkinCtrl := chkCtrl.NewCheckinController(checkinManual, checkinPin, checkinOverride, checkinRepo, uow, nil, tokens)
	reportsController := reportsCtrl.NewReportsController(dashboard, attentionRequired, rangeReport, exportReport, markContacted, markLost, tokens)
	notificationsCtrl := notiCtrl.NewController(connectWhatsApp, whatsappStatus, listTemplates, updateTemplate, broadcast, listNotifications, listOwnerAlerts, updateOwnerAlert, whatsappProvider, tokens)

	// ── Subscriptions (Fase 1: cobranza al dueño) ─────────────────────────
	subEventRepo := subDB.NewEventPostgresRepository()
	recordSubEvent := subApp.NewRecordEvent(subEventRepo, gymRepo, uow, recorder)
	getSubscription := subApp.NewGetSubscription(subEventRepo, gymRepo, uow)
	subVerifier := subCtrl.NewWebhookVerifier(
		envOrDefault("STRIPE_WEBHOOK_SECRET", ""),
		envOrDefault("MERCADOPAGO_WEBHOOK_SECRET", ""),
		os.Getenv("ENVIRONMENT") == "production",
	)
	subGateways := buildSubscriptionGateways()
	billingSuccessURL := envOrDefault("BILLING_SUCCESS_URL", baseURL+"/settings/billing?status=success")
	billingCancelURL := envOrDefault("BILLING_CANCEL_URL", baseURL+"/settings/billing?status=cancelled")
	startCheckout := subApp.NewStartCheckout(subGateways, gymRepo, userRepo, uow, billingSuccessURL, billingCancelURL)
	subscriptionsCtrl := subCtrl.NewSubscriptionController(recordSubEvent, getSubscription, startCheckout, subVerifier, tokens)
	twilioWebhookURL := envOrDefault("TWILIO_WEBHOOK_URL", baseURL+"/api/v1/webhooks/twilio")
	notiWebhookCtrl := notiCtrl.NewWebhookController(processWebhook, envOrDefault("TWILIO_AUTH_TOKEN", ""), twilioWebhookURL)

	// ── Gin router ────────────────────────────────────────────────────────
	if os.Getenv("ENVIRONMENT") == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.Default()
	r.Use(middleware.CORS(middleware.CORSConfig{
		AllowedOrigins: parseOrigins(envOrDefault("CORS_ALLOWED_ORIGINS", "http://localhost:5174")),
	}))
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "cuadra-server"})
	})
	authCtrl.RegisterRoutes(r)
	authCtrl.RegisterUploadsRoute(r)
	mtCtrl.RegisterRoutes(r)
	memberCtrl.RegisterRoutes(r)
	fingerprintCtrl.RegisterRoutes(r)
	paymentCtrl.RegisterRoutes(r)
	productCtrl.RegisterRoutes(r)
	checkinCtrl.RegisterRoutes(r)
	reportsController.RegisterRoutes(r)
	notificationsCtrl.RegisterRoutes(r)
	notiWebhookCtrl.RegisterRoutes(r)
	subscriptionsCtrl.RegisterRoutes(r)

	// Sync protocol (Sesión 8 / ADR-001) — push/pull/full + Prometheus
	// metrics at /_internal/metrics. The handler depends only on the UoW
	// already wired above; Store and ConflictLogger are stateless.
	syncMetrics := syncShared.NewMetrics()
	syncStore := syncShared.NewPostgresStore()
	syncConflicts := syncShared.NewConflictLogger()
	syncHandler := syncShared.NewHandler(uow, syncStore, syncConflicts, tokens, sidecarStore, syncMetrics)
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

func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

// buildSubscriptionGateways registers a CheckoutGateway for each processor
// whose creds are present. Missing creds → that provider stays absent from
// the map; the use case maps that to ErrGatewayUnavailable so the FE shows
// "this method isn't available" instead of crashing.
//
// Required for Stripe: STRIPE_SECRET_KEY + STRIPE_PRICE_STANDARD/PLUS (price
// ids created in the Stripe MX dashboard). Required for MP: MP_ACCESS_TOKEN
// + MP_AMOUNT_STANDARD/PLUS (amounts in MXN per month).
func buildSubscriptionGateways() map[subDomain.Provider]subDomain.CheckoutGateway {
	out := map[subDomain.Provider]subDomain.CheckoutGateway{}
	if g := subPay.NewStripeGateway(subPay.StripeConfig{
		SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		PriceStandard: os.Getenv("STRIPE_PRICE_STANDARD"),
		PricePlus:     os.Getenv("STRIPE_PRICE_PLUS"),
	}); g != nil {
		out[subDomain.ProviderStripe] = g
	} else {
		log.Printf("[subscriptions] STRIPE_SECRET_KEY missing — Stripe checkout disabled")
	}
	if g := subPay.NewMercadoPagoGateway(subPay.MercadoPagoConfig{
		AccessToken:    os.Getenv("MP_ACCESS_TOKEN"),
		AmountStandard: envFloat("MP_AMOUNT_STANDARD", 0),
		AmountPlus:     envFloat("MP_AMOUNT_PLUS", 0),
		BackURL:        os.Getenv("MP_BACK_URL"),
	}); g != nil {
		out[subDomain.ProviderMercadoPago] = g
	} else {
		log.Printf("[subscriptions] MP_ACCESS_TOKEN missing — Mercado Pago checkout disabled")
	}
	return out
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
			OTPFromNumber:     envOrDefault("TWILIO_OTP_FROM", ""),
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
