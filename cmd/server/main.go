//go:build server

package main

import (
	"log"
	"net/http"
	"os"
	"strconv"

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

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/email"
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

	// ── Shared services ────────────────────────────────────────────────────
	tokens := auth.NewJWTService(mustEnv("JWT_SECRET"))
	recorder := audit.NewPostgresRecorder()
	emailSender := email.NewStdoutSender()
	trialDays := envInt("TRIAL_DURATION_DAYS", 30)
	baseURL := envOrDefault("PUBLIC_BASE_URL", "https://cuadra.app")

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

	// ── Billing (Sesión 3) ────────────────────────────────────────────────
	folios := folioSvc.NewGenerator(paymentRepo)
	registerPayment := billingApp.NewRegisterMembershipPayment(paymentRepo, folios, memberSvc, memberRepo, uow, recorder, billingApp.NoopPublisher{})
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
	registerSale := billingApp.NewRegisterSale(paymentRepo, saleRepo, saleItemRepo, folios, productSvc, memberRepo, uow, recorder, billingApp.NoopPublisher{})
	refundSale := billingApp.NewRefundSale(saleRepo, refundPayment, uow)
	cashClose := reportsApp.NewCashClose(cashCloseReader, cashCloseEventRepo, uow, recorder)

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
