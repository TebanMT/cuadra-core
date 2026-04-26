//go:build sidecar

package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"

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

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/email"
	"github.com/cuadra/cuadra-core/src/shared/sync"
)

func main() {
	_ = godotenv.Load()

	dbPath := envOrDefault("SIDECAR_DB_PATH", "./tmp/cuadra.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}
	db := infraDB.InitSQLite(dbPath)
	defer infraDB.CloseSQLite()

	if err := infraDB.ApplySQLiteMigrations(db, envOrDefault("MIGRATIONS_DIR", "db_migrations/sqlite")); err != nil {
		log.Fatalf("apply sqlite migrations: %v", err)
	}

	queue := sync.NewSqliteQueue()
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

	// ── Shared services ────────────────────────────────────────────────────
	tokens := auth.NewJWTService(envOrDefault("JWT_SECRET", "sidecar-dev-secret-do-not-use-in-prod"))
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
	})
	mtCtrl := memCtrl.NewMembershipTypeController(createMT, updateMT, deactivateMT, listMT, tokens)
	memberCtrl := memCtrl.NewMemberController(createMember, updateMember, listMembers, memberDetail, toggleMember, lockExpiry, assignPin, tokens)
	fingerprintCtrl := memCtrl.NewFingerprintController(registerFingerprint, tokens)
	paymentCtrl := billingCtrl.NewPaymentController(registerPayment, settlePayment, receiptPayment, sendReceipt, listMemberPayments, refundPayment, registerSale, refundSale, cashClose, tokens)
	productCtrl := prodCtrl.NewProductController(createProduct, updateProduct, deactivateProduct, listProducts, adjustStock, tokens)
	checkinCtrl := chkCtrl.NewCheckinController(checkinManual, checkinPin, checkinOverride, tokens)
	kioskCtrl := chkCtrl.NewKioskController(checkinFingerprint, kioskLoop, kioskEvents, bioReader, tokens)

	if os.Getenv("ENVIRONMENT") == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.Default()
	r.Use(localTokenMiddleware(envOrDefault("LOCAL_AUTH_TOKEN", "")))
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "cuadra-sidecar"})
	})
	authCtrl.RegisterRoutes(r)
	mtCtrl.RegisterRoutes(r)
	memberCtrl.RegisterRoutes(r)
	fingerprintCtrl.RegisterRoutes(r)
	paymentCtrl.RegisterRoutes(r)
	productCtrl.RegisterRoutes(r)
	checkinCtrl.RegisterRoutes(r)
	kioskCtrl.RegisterRoutes(r)

	port := envOrDefault("SIDECAR_PORT", "9090")
	// ADR-003 §2.2: print the port to stdout so Tauri can capture it.
	fmt.Printf("LISTENING_ON=%s\n", port)
	log.Printf("cuadra-sidecar starting on 127.0.0.1:%s db=%s", port, dbPath)
	if err := r.Run("127.0.0.1:" + port); err != nil {
		log.Fatalf("gin: %v", err)
	}
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
