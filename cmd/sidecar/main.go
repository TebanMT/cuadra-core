//go:build sidecar

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
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

	challengesApp "github.com/cuadra/cuadra-core/src/modules/challenges/app"
	challengesInfra "github.com/cuadra/cuadra-core/src/modules/challenges/infraestructure"
	challengesRepoLite "github.com/cuadra/cuadra-core/src/modules/challenges/infraestructure/db/repositories"
	challengesCtrl "github.com/cuadra/cuadra-core/src/modules/challenges/interfaces/controllers"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	billingRepoLite "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	billingCtrl "github.com/cuadra/cuadra-core/src/modules/billing/interfaces/controllers"

	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	prodRepoLite "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	prodCtrl "github.com/cuadra/cuadra-core/src/modules/products/interfaces/controllers"

	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	expRepoLite "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	expCtrl "github.com/cuadra/cuadra-core/src/modules/expenses/interfaces/controllers"

	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	usersCtrl "github.com/cuadra/cuadra-core/src/modules/users/interfaces/controllers"

	promoApp "github.com/cuadra/cuadra-core/src/modules/promotions/app"
	promoRepoLite "github.com/cuadra/cuadra-core/src/modules/promotions/infraestructure/db/repositories"
	promoCtrl "github.com/cuadra/cuadra-core/src/modules/promotions/interfaces/controllers"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notiRepoLite "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/repositories"
	notiEmail "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/email"
	notiWhatsApp "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/whatsapp"
	notiCtrl "github.com/cuadra/cuadra-core/src/modules/notifications/interfaces/controllers"

	subApp "github.com/cuadra/cuadra-core/src/modules/subscriptions/app"
	subRepoLite "github.com/cuadra/cuadra-core/src/modules/subscriptions/infraestructure/db/repositories"
	subCtrl "github.com/cuadra/cuadra-core/src/modules/subscriptions/interfaces/controllers"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	reportsCtrl "github.com/cuadra/cuadra-core/src/application/reports/interfaces"
	"github.com/cuadra/cuadra-core/src/shared/accesswebhook"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/audit/audithttp"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/email"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/runtime"
	syncShared "github.com/cuadra/cuadra-core/src/shared/sync"
)

// version es la versión del build, estampada al compilar vía
// -ldflags "-X main.version=<tag>". "dev" es el default para builds
// locales sin estampar. Se expone en GET /health. (El sidecar lo bundlea
// cuadra-desktop en su propio CI; la inyección de versión allá es futura.)
var version = "dev"

func main() {
	_ = godotenv.Load()

	// Parent-death watcher: when the desktop app exits irregularly
	// (Cmd+Q on macOS, force-quit, crash) the OS does NOT propagate
	// SIGTERM to child processes — the sidecar would otherwise live
	// on as an orphan reparented to launchd, holding port 9090. The
	// next desktop launch then can't bind the port and "sidecar not
	// ready" surfaces in the UI.
	//
	// The portable workaround is for the sidecar itself to poll the
	// parent PID and exit when it changes (the parent has died and
	// we've been reparented). On Linux/BSD we could use prctl
	// PR_SET_PDEATHSIG, but on macOS there's no kernel-level
	// equivalent — so we just watch.
	//
	// Skipped when launched standalone (parent is the user's shell)
	// via the SIDECAR_NO_PARENT_WATCH env var, used by integration
	// tests and ad-hoc CLI invocations.
	if os.Getenv("SIDECAR_NO_PARENT_WATCH") == "" {
		startParentWatcher()
	}

	// Persistent storage paths. El desktop Tauri normalmente inyecta
	// SIDECAR_DB_PATH / UPLOADS_DIR apuntando al app-data dir del OS;
	// defaultSidecarDataDir() es el fallback para runs standalone y para
	// cualquier caso donde el env no llegue. Nunca un ./tmp relativo: esa
	// DB es la operación completa del gym (no es scratch), y un path
	// relativo revienta cuando el sidecar corre desde un install dir
	// no-escribible (ej. C:\Program Files\Tinta).
	dataDir := defaultSidecarDataDir()
	dbPath := envOrDefault("SIDECAR_DB_PATH", filepath.Join(dataDir, "tinta.db"))
	uploadsDir := envOrDefault("UPLOADS_DIR", filepath.Join(dataDir, "uploads"))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}

	// Mirror the standard logger to a file next to the DB. The desktop
	// captures the sidecar's stdout but routes it through env_logger to
	// stderr, which is invisible in a packaged GUI app — a file is the only
	// way to read [biometric] match scores and other diagnostics in the
	// field. Truncated each start so it stays bounded (one session per file).
	logPath := filepath.Join(filepath.Dir(dbPath), "sidecar.log")
	if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	} else {
		log.Printf("could not open sidecar log file %q: %v", logPath, err)
	}
	log.Printf("sidecar log file: %s", logPath)

	logRuntimeMode()

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
	expenseRepo := expRepoLite.NewExpenseSQLiteRepository()
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

	// Biometric reader (UC-028, UC-029, UC-031). The sidecar build wires the
	// NBIS matcher (mindtct + bozorth3 subprocesses, ADR-004-bis); the mock
	// lives behind `bio_mock` for tests and is consumed via NewMockReader in
	// the test fixtures, not here.
	//
	// GMKs go to the OS keychain (Windows Credential Manager / macOS
	// Keychain) per ADR-006 §2.6 so a sidecar restart doesn't orphan every
	// enrolled gallery. KeyringGMKProvider lazy-generates the per-gym key on
	// first GetGMK and caches in-process. Override with SIDECAR_GMK_INMEMORY=1
	// for ad-hoc tests on a box without an OS keyring (CI runners that
	// don't keep a logged-in user, headless Linux dev VMs).
	var gmkProvider bcrypto.GMKProvider
	if os.Getenv("SIDECAR_GMK_INMEMORY") == "1" {
		log.Printf("[biometric] SIDECAR_GMK_INMEMORY=1 — using in-memory GMK provider (galleries die at restart)")
		gmkProvider = bcrypto.NewInMemoryGMKProvider()
	} else {
		gmkProvider = bcrypto.NewKeyringGMKProvider()
	}
	bioReader := biometric.NewDigitalPersonaReader().WithGMK(gmkProvider)

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
	updateChargeSettings := gymApp.NewUpdateChargeSettings(gymRepo, uow, recorder)
	createOp := usersApp.NewCreateOperator(userRepo, uow, recorder).WithGymRepo(gymRepo)
	updateOp := usersApp.NewUpdateOperator(userRepo, uow, recorder)
	toggleOp := usersApp.NewToggleOperatorActive(userRepo, nil, uow, recorder)
	resetOp := usersApp.NewResetOperatorPassword(userRepo, nil, uow, recorder)
	rotateOpPIN := usersApp.NewRotateOperatorPIN(userRepo, uow, recorder)
	requestTransfer := usersApp.NewRequestTransferOwnership(userRepo, otpRepo, uow, recorder, emailSender)
	confirmTransfer := usersApp.NewConfirmTransferOwnership(userRepo, otpRepo, transferRepo, nil, uow, recorder, emailSender)
	// PIN use cases (auth-refactor v0.7). assignSelfPIN + clearSelfPIN power
	// POST/DELETE /auth/me/pin and write locally + sync-queue (same posture
	// as CreateOperator); the sync agent pushes the row to cloud where the
	// projector applies it back to other sidecars in the same gym.
	assignSelfPIN := usersApp.NewAssignSelfPIN(userRepo, uow, recorder)
	clearSelfPIN := usersApp.NewClearSelfPIN(userRepo, uow, recorder)
	loginPIN := usersApp.NewLoginPIN(userRepo, gymRepo, uow, tokens, recorder)
	listOperatorsForLogin := usersApp.NewListOperatorsForLogin(userRepo, uow)
	createMT := memApp.NewCreateMembershipType(mtRepo, uow, recorder)
	updateMT := memApp.NewUpdateMembershipType(mtRepo, uow, recorder)
	deactivateMT := memApp.NewDeactivateMembershipType(mtRepo, uow, recorder)
	listMT := memApp.NewListMembershipTypes(mtRepo, uow)
	// Folios para que CreateMember pueda registrar el primer pago en su
	// propia tx (sin pasar por UC-018, que renovaría la membresía recién
	// creada). Mismo generator que usa billing más abajo.
	folios := folioSvc.NewGenerator(paymentRepo)
	createMember := memApp.NewCreateMemberWithBilling(memberRepo, membershipRepo, mtRepo, paymentRepo, folios, uow, recorder)
	updateMember := memApp.NewUpdateMember(memberRepo, uow, recorder)
	listMembers := memApp.NewListMembers(memberRepo, uow)
	memberDetail := memApp.NewGetMemberDetail(memberRepo, fingerprintRepo, uow)
	toggleMember := memApp.NewToggleMemberStatus(memberRepo, uow, recorder)
	lockExpiry := memApp.NewLockMembershipExpiry(membershipRepo, adjustmentRepo, uow, recorder)
	assignNumber := memApp.NewAssignMemberNumber(memberRepo, uow, recorder)
	// ADR-010: la longitud del número de socio (y su bump al ~50%) vive en el
	// config del gym; este seam la lee/crece dentro de la tx del alta/asignación.
	memberNumberCfg := gymApp.NewMemberNumberConfig(gymRepo)
	createMember.WithMemberNumberDigits(memberNumberCfg)
	assignNumber.WithDigitsStore(memberNumberCfg)
	importCSV := memApp.NewImportMembersFromCSV(memberRepo, membershipRepo, mtRepo, uow, recorder)
	memberSvc := memApp.NewMemberService(memberRepo, membershipRepo, mtRepo).
		WithFingerprints(fingerprintRepo).
		WithAdjustments(adjustmentRepo)

	// ── Promotions (Standard) ─────────────────────────────────────────────
	promotionRepo := promoRepoLite.NewPromotionSQLiteRepository()
	appliedPromoRepo := promoRepoLite.NewAppliedPromotionSQLiteRepository()
	createPromo := promoApp.NewCreatePromotion(promotionRepo, uow, recorder)
	updatePromo := promoApp.NewUpdatePromotion(promotionRepo, uow, recorder)
	deactivatePromo := promoApp.NewDeactivatePromotion(promotionRepo, uow, recorder)
	reactivatePromo := promoApp.NewReactivatePromotion(promotionRepo, uow, recorder)
	listPromos := promoApp.NewListPromotions(promotionRepo, uow)
	getPromoByCode := promoApp.NewGetPromotionByCode(promotionRepo, appliedPromoRepo, uow)
	applyPromo := promoApp.NewApplyPromotion(promotionRepo, appliedPromoRepo)
	listAppliedByMonth := promoApp.NewListAppliedByMonth(appliedPromoRepo, uow)
	createMember.WithPromotions(applyPromo, memberSvc)

	// ── Biometric + Checkins (Sesión 5) ───────────────────────────────────
	// Sidecar wires the real Reader so UC-028's pre-enrollment collision
	// check runs (production enrollment lives here, not in cloud).
	registerFingerprint := memApp.NewRegisterFingerprint(memberRepo, fingerprintRepo, gmkProvider, uow, recorder).WithReader(bioReader)
	deleteFingerprint := memApp.NewDeleteFingerprint(memberRepo, fingerprintRepo, uow, recorder)
	checkinManual := chkApp.NewCheckinManual(memberSvc, checkinRepo, uow, recorder).WithGyms(gymRepo)
	checkinNumber := chkApp.NewCheckinByNumber(memberSvc, memberRepo, checkinRepo, uow, recorder, nil).WithGyms(gymRepo)
	checkinOverride := chkApp.NewOverrideCheckin(memberSvc, checkinRepo, uow, recorder).WithGyms(gymRepo)
	checkinFingerprint := chkApp.NewCheckinByFingerprint(memberSvc, checkinRepo, bioReader, uow, recorder).WithGyms(gymRepo)
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
	// appBaseURL es la URL pública del frontend (página del comprobante).
	enqueueReceipt := notiApp.NewEnqueueReceipt(notificationRepo, gymRepo, memberRepo, templateRepo, uow)
	enqueueWelcomePin := notiApp.NewEnqueueWelcomePin(notificationRepo, gymRepo, memberRepo)
	createMember.WithWelcomeNotifier(enqueueWelcomePin)
	createMember.WithReceiptNotifier(notiApp.NewFirstPaymentReceiptNotifier(enqueueReceipt))
	assignNumber.WithWelcomeNotifier(enqueueWelcomePin)
	// Operator PIN-first: alta y rotación encolan WhatsApp con el PIN.
	// Sidecar registra la noti localmente y el sync agent la empuja a
	// cloud — el worker cloud despacha el outbound. Si el gym no tiene
	// WhatsApp conectado, el notifier hace skip silencioso.
	enqueueOperatorWelcomePIN := notiApp.NewEnqueueOperatorWelcomePIN(notificationRepo, gymRepo, userRepo)
	createOp.WithWelcomePINNotifier(enqueueOperatorWelcomePIN)
	rotateOpPIN.WithWelcomePINNotifier(enqueueOperatorWelcomePIN)
	enqueueOwnerAlert := notiApp.NewEnqueueOwnerAlert(notificationRepo, gymRepo, userRepo, alertConfigRepo, uow)
	// El ceremony de UC-037 (connect/disconnect) ya NO se cablea aquí: es
	// cloud-authoritative (necesita Twilio real + internet; el mock local
	// "registraba" senders contra stdout y el push nunca propagaba los
	// campos). El desktop llega vía WhatsAppSidecarProxy, que forwardea al
	// cloud con el sk_live_* — ver el registro de rutas más abajo. El GET
	// de status sí sigue local (lee el SQLite sincronizado).
	whatsappStatus := notiApp.NewGetWhatsAppStatus(gymRepo, notificationRepo, uow)
	listTemplates := notiApp.NewListTemplates(templateRepo, uow)
	updateTemplate := notiApp.NewUpdateTemplate(templateRepo, uow, recorder)
	listOwnerAlerts := notiApp.NewListOwnerAlerts(alertConfigRepo, templateRepo, uow)
	updateOwnerAlert := notiApp.NewUpdateOwnerAlert(alertConfigRepo, templateRepo, uow, recorder)
	// Gate de personalización de copy. Ver cmd/server/main.go: el texto sólo se
	// edita cuando el gym usa su PROPIO número (UsesOwnWhatsAppNumber); si sale
	// por el maestro de Tinta, lo que llega es la plantilla aprobada por Meta,
	// así que el use case fuerza el body al default y sólo respeta el switch
	// on/off. Este es el path real de edición — la recepción pega al sidecar.
	canEditTemplateBody := func(ctx context.Context, gymID uuid.UUID) bool {
		tx, err := uow.Query(ctx)
		if err != nil {
			return true
		}
		g, err := gymRepo.GetByID(tx, gymID)
		if err != nil || g == nil {
			return true
		}
		return g.UsesOwnWhatsAppNumber()
	}
	updateTemplate.CanEditBody = canEditTemplateBody
	updateOwnerAlert.CanEditBody = canEditTemplateBody
	broadcast := notiApp.NewBroadcast(notificationRepo, memberRepo, gymRepo, audit.NewSQLiteReader(), uow, recorder)
	listNotifications := notiApp.NewListNotifications(notificationRepo, uow)
	retryNotification := notiApp.NewRetryNotification(notificationRepo, uow, recorder)
	billingSubscriber := notiApp.NewBillingEventSubscriber(enqueueReceipt)
	_ = emailMock

	// ── Billing (Sesión 3) ────────────────────────────────────────────────
	// `folios` se construyó arriba (lo reusa createMember). Mismo generator.
	registerPayment := billingApp.NewRegisterMembershipPayment(paymentRepo, folios, memberSvc, memberRepo, uow, recorder, billingSubscriber).
		WithPromotions(applyPromo).
		WithWelcomeNotifier(enqueueWelcomePin).
		WithGyms(gymRepo)
	settlePayment := billingApp.NewSettlePendingBalance(paymentRepo, folios, uow, recorder)
	receiptPayment := billingApp.NewGenerateReceipt(paymentRepo, gymRepo, memberRepo, uow)
	sendReceipt := billingApp.NewSendReceipt(paymentRepo, uow).WithPublisher(billingSubscriber).WithResender(billingSubscriber)
	listMemberPayments := billingApp.NewListMemberPayments(paymentRepo, memberRepo, uow)
	listGymPayments := billingApp.NewListGymPayments(paymentRepo, memberRepo, uow)
	refundPayment := billingApp.NewRefundPayment(paymentRepo, folios, memberSvc, uow, recorder).
		WithGyms(gymRepo)

	// ── Products + Billing pt.2 (Sesión 4) ────────────────────────────────
	productSvc := prodApp.NewProductService(productRepo, stockMovementRepo)
	createProduct := prodApp.NewCreateProduct(productRepo, stockMovementRepo, uow, recorder)
	updateProduct := prodApp.NewUpdateProduct(productRepo, uow, recorder)
	deactivateProduct := prodApp.NewDeactivateProduct(productRepo, uow, recorder)
	reactivateProduct := prodApp.NewReactivateProduct(productRepo, uow, recorder)
	listProducts := prodApp.NewListProducts(productRepo, uow)
	adjustStock := prodApp.NewAdjustStock(productRepo, stockMovementRepo, uow, recorder)
	// Expenses (gastos generales) — CRUD + listado. Mismo wiring que cloud.
	createExpense := expApp.NewCreateExpense(expenseRepo, uow, recorder)
	updateExpense := expApp.NewUpdateExpense(expenseRepo, uow, recorder)
	deleteExpense := expApp.NewDeleteExpense(expenseRepo, uow, recorder)
	listExpenses := expApp.NewListExpenses(expenseRepo, uow)
	registerSale := billingApp.NewRegisterSale(paymentRepo, saleRepo, saleItemRepo, folios, productSvc, memberRepo, uow, recorder, billingSubscriber).
		WithPromotions(applyPromo).
		WithGyms(gymRepo)
	refundSale := billingApp.NewRefundSale(saleRepo, refundPayment, uow)
	cashClose := reportsApp.NewCashClose(cashCloseReader, cashCloseEventRepo, uow, recorder).
		WithExpenses(expenseRepo).
		WithUsers(userRepo).
		WithSubscriber(notiApp.NewCashCloseAlertSubscriber(enqueueOwnerAlert))

	// ── Reports application layer (Sesión 6) — same use cases as the cloud,
	// but reading from the local SQLite. TTL corto (5s, vs 60s del cloud):
	// acá las queries pegan a SQLite local (milisegundos) y un TTL largo
	// hacía que el dashboard siguiera viejo hasta 60s DESPUÉS de que el FE
	// refetcheara tras un pull con cambios — el operador lo veía como "el
	// sync no actualizó nada". 5s sólo coalesce ráfagas (kiosko + dashboard
	// pidiendo a la vez); el cloud conserva 60s porque ahí protege Postgres.
	dashboard := reportsApp.NewDashboard(reportsReader, uow, 5*time.Second)
	attentionRequired := reportsApp.NewAttentionRequired(reportsReader, uow)
	rangeReport := reportsApp.NewRangeReport(reportsReader, uow)
	exportReport := reportsApp.NewExportReport(reportsReader, gymRepo, uow, attentionRequired, rangeReport)
	genderReport := reportsApp.NewGenderReport(reportsReader, uow)
	markContacted := memApp.NewMarkContacted(memberRepo, contactAttemptRepo, uow, recorder)
	markLost := memApp.NewMarkLost(memberRepo, uow, recorder)

	// ── Challenges (retos) — Sesión 2 ─────────────────────────────────────
	// Full vertical slice running locally on the gym laptop. AttendanceCounter
	// reads from the same SQLite checkins table; the sync agent flushes new
	// rows cloud-side without changing the in-gym UX.
	challengeRepo := challengesRepoLite.NewChallengeSQLiteRepository()
	categoryRepo := challengesRepoLite.NewCategorySQLiteRepository()
	participantRepo := challengesRepoLite.NewParticipantSQLiteRepository()
	measurementRepo := challengesRepoLite.NewMeasurementSQLiteRepository()
	attendanceCounter := challengesInfra.NewCheckinsAttendanceAdapter()
	createChallenge := challengesApp.NewCreateChallenge(challengeRepo, uow, recorder)
	listChallenges := challengesApp.NewListChallenges(challengeRepo, uow)
	detailChallenge := challengesApp.NewGetChallengeDetail(challengeRepo, categoryRepo, participantRepo, measurementRepo, uow)
	updateChallengeConfig := challengesApp.NewUpdateChallengeConfig(challengeRepo, measurementRepo, uow, recorder)
	transitionChallenge := challengesApp.NewTransitionChallengeStatus(challengeRepo, categoryRepo, uow, recorder)
	addCategory := challengesApp.NewAddCategory(challengeRepo, categoryRepo, uow, recorder)
	updateCategoryUC := challengesApp.NewUpdateCategory(categoryRepo, uow, recorder)
	deleteCategory := challengesApp.NewDeleteCategory(categoryRepo, uow, recorder)
	listCategories := challengesApp.NewListCategories(challengeRepo, categoryRepo, uow)
	addParticipant := challengesApp.NewAddParticipant(challengeRepo, categoryRepo, participantRepo, uow, recorder)
	updateParticipant := challengesApp.NewUpdateParticipant(participantRepo, uow, recorder)
	removeParticipant := challengesApp.NewRemoveParticipant(participantRepo, uow, recorder)
	listParticipants := challengesApp.NewListParticipants(challengeRepo, participantRepo, uow)
	captureMeasurement := challengesApp.NewCaptureMeasurement(challengeRepo, participantRepo, measurementRepo, uow, recorder)
	listMeasurementsUC := challengesApp.NewListMeasurements(challengeRepo, participantRepo, measurementRepo, uow)
	rankingUC := challengesApp.NewGetChallengeRanking(challengeRepo, participantRepo, measurementRepo, attendanceCounter, uow)
	attendanceReport := challengesApp.NewGetAttendanceReport(challengeRepo, participantRepo, attendanceCounter, uow)
	checkDQ := challengesApp.NewCheckDisqualifications(challengeRepo, participantRepo, attendanceCounter, uow, recorder)

	authCtrl := usersCtrl.NewAuthController(usersCtrl.AuthController{
		Signup:            signup,
		Login:             login,
		Logout:            logout,
		UpdateBasicInfo:   updateBasic,
		UpdatePayMethods:  updatePay,
		CompleteSetup:     completeSetup,
		UpdateProfile:     updateProfile,
		UpdateChargeSet:   updateChargeSettings,
		CreateOperator:    createOp,
		UpdateOperator:    updateOp,
		ToggleActive:      toggleOp,
		ResetOpPassword:   resetOp,
		RotateOperatorPIN: rotateOpPIN,
		RequestTransfer:   requestTransfer,
		ConfirmTransfer:   confirmTransfer,
		AssignSelfPIN:     assignSelfPIN,
		ClearSelfPIN:      clearSelfPIN,
		Tokens:            tokens,
		Gyms:              gymRepo,
		Users:             userRepo,
		MembershipTypes:   mtRepo,
		UoW:               uow,
		UploadsDir:        uploadsDir,
	})
	mtCtrl := memCtrl.NewMembershipTypeController(createMT, updateMT, deactivateMT, listMT, tokens)
	promotionsCtrl := promoCtrl.NewPromotionController(createPromo, updatePromo, deactivatePromo, reactivatePromo, listPromos, getPromoByCode, listAppliedByMonth, tokens)
	// PlanGate aplica en el sidecar igual que en cloud: el SKU del gym
	// vive en el mirror local (sync agent lo mantiene fresco), así que el
	// gate funciona offline.
	plusGate := middleware.RequirePlusPlan(gymRepo, uow)

	memberCtrl := memCtrl.NewMemberController(createMember, updateMember, listMembers, memberDetail, toggleMember, lockExpiry, assignNumber, tokens).
		WithUploadsDir(uploadsDir).
		WithImportCSV(importCSV)
	// SyncTrigger se setea más abajo cuando el agente está construido
	// (orden temporal: el agente necesita el cloudURL y la config, que
	// llegan después de los controllers).
	fingerprintCtrl := memCtrl.NewFingerprintController(registerFingerprint, deleteFingerprint, tokens)
	paymentCtrl := billingCtrl.NewPaymentController(registerPayment, settlePayment, receiptPayment, sendReceipt, listMemberPayments, listGymPayments, refundPayment, registerSale, refundSale, cashClose, tokens)
	paymentCtrl.PlanGate = plusGate
	productCtrl := prodCtrl.NewProductController(createProduct, updateProduct, deactivateProduct, reactivateProduct, listProducts, adjustStock, tokens)
	expenseController := expCtrl.NewExpenseController(createExpense, updateExpense, deleteExpense, listExpenses, tokens)
	expenseController.PlanGate = plusGate
	fingerprintAvailable := func() bool { return bioReader.Available(context.Background()) }
	// Outbound access-granted webhook (Fase 1 differentiator: lets the gym
	// drive any turnstile / cerradura over HTTP). URL + HMAC secret live in
	// gyms.kiosk_settings; dispatcher reads them per-call.
	accessWebhook := accesswebhook.NewHTTPDispatcher(uow, gymRepo)
	checkinCtrl := chkCtrl.NewCheckinController(checkinManual, checkinNumber, checkinOverride, checkinRepo, uow, fingerprintAvailable, tokens).
		WithWebhook(accessWebhook)
	kioskCtrl := chkCtrl.NewKioskController(checkinFingerprint, kioskLoop, bioReader, tokens).
		WithSibling(checkinCtrl)
	// New PNG-in HTTP surface per ADR-004-bis: the Tauri frontend captures
	// via the DigitalPersona JS SDK and POSTs the raw image; the sidecar
	// runs ExtractTemplate + the use case here. The base64-in routes on
	// fingerprintCtrl + kioskCtrl stay for kiosk-loop and test callers.
	biometricCtrl := chkCtrl.NewBiometricController(bioReader, registerFingerprint, checkinFingerprint, tokens).
		WithSibling(checkinCtrl)
	// Connect/Disconnect nil → el controller compartido NO registra las
	// rutas del ceremony; las cubre el WhatsAppSidecarProxy (proxy al cloud).
	notificationsCtrl := notiCtrl.NewController(nil, nil, whatsappStatus, listTemplates, updateTemplate, broadcast, listNotifications, retryNotification, listOwnerAlerts, updateOwnerAlert, whatsappMock, tokens)
	notificationsCtrl.PlanGate = plusGate

	// Suscripción — sidecar sólo expone el GET. El historial ahora SÍ vive
	// local: subscription_events es una SyncedTable pull-only (cloud → sidecar)
	// que el sync agent llena. Checkout/extend-trial siguen cloud-only — el
	// FE abre el dashboard cloud para ese flujo via openExternal (decisión
	// del riesgo 1: pagos siempre suceden en browser conectado a internet,
	// proxy-ear sólo agregaría complejidad sin ganancia de UX).
	subEvents := subRepoLite.NewEventSQLiteRepository()
	getSubscription := subApp.NewGetSubscription(subEvents, gymRepo, uow)
	subscriptionCtrl := subCtrl.NewSubscriptionController(nil, getSubscription, nil, nil, nil, tokens)

	// Bitácora (item 9) — owner-only. El sidecar lee del audit_log local
	// (recordado por cada use case en cada gym). El reader concreto lo
	// elige el build tag (NewSQLiteReader sólo compila bajo //go:build sidecar).
	auditCtrl := audithttp.NewController(audit.NewSQLiteReader(), userRepo, uow, tokens)
	auditCtrl.PlanGate = plusGate
	reportsController := reportsCtrl.NewReportsController(dashboard, attentionRequired, rangeReport, exportReport, markContacted, markLost, tokens).
		WithGenderReport(genderReport)
	reportsController.PlanGate = plusGate
	challengeCtrl := challengesCtrl.NewChallengeController(challengesCtrl.ChallengeController{
		CreateChallenge:        createChallenge,
		ListChallenges:         listChallenges,
		GetChallengeDetail:     detailChallenge,
		UpdateChallengeConfig:  updateChallengeConfig,
		TransitionStatus:       transitionChallenge,
		AddCategory:            addCategory,
		UpdateCategory:         updateCategoryUC,
		DeleteCategory:         deleteCategory,
		ListCategories:         listCategories,
		AddParticipant:         addParticipant,
		UpdateParticipant:      updateParticipant,
		RemoveParticipant:      removeParticipant,
		ListParticipants:       listParticipants,
		CaptureMeasurement:     captureMeasurement,
		ListMeasurements:       listMeasurementsUC,
		GetChallengeRanking:    rankingUC,
		GetAttendanceReport:    attendanceReport,
		CheckDisqualifications: checkDQ,
		Tokens:                 tokens,
		PlanGate:               plusGate,
	})

	if os.Getenv("ENVIRONMENT") == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.Default()
	r.Use(middleware.CORS(middleware.CORSConfig{
		AllowedOrigins: parseOrigins(envOrDefault("CORS_ALLOWED_ORIGINS", "http://localhost:5173,tauri://localhost,http://tauri.localhost")),
	}))
	r.Use(localTokenMiddleware(envOrDefault("LOCAL_AUTH_TOKEN", "")))
	// Hard-block por suscripción vencida (riesgo 1 — decisión "offline tolera,
	// sync exitoso confirma cancelación"). Va a nivel engine para cubrir TODA
	// la API sin tener que modificar cada controller. El middleware tiene su
	// propia whitelist de paths que SIEMPRE deben pasar (auth/*, gyms/me read,
	// subscription/me read, sync/*) para que la pantalla de bloqueo del FE
	// pueda renderearse y el dueño pueda escapar al dashboard cloud.
	r.Use(middleware.EnforceActiveSubscription(tokens, gymRepo, uow))
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "cuadra-sidecar", "version": version})
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
		CloudURL:      cloudURL,
		UoW:           uow,
		LocalTokens:   tokens,
		ClientID:      clientID,
		DeviceLabel:   deviceLabel,
		LoginPIN:      loginPIN,
		ListOperators: listOperatorsForLogin,
		// LoginGymID reads the cached_login row each call so the operators
		// grid follows whatever gym this sidecar is currently paired to.
		// Returns uuid.Nil when the sidecar hasn't been redeemed yet —
		// the handler treats that as "no operators".
		LoginGymID: func() uuid.UUID {
			return loginGymIDFromCache(uow)
		},
	})
	// Bind the biometric matcher's active gym to the auth flow so Identify
	// can fetch the right GMK without each request having to pass it in.
	// Logout passes uuid.Nil and the matcher refuses to decrypt.
	authProxy.OnActiveGymChanged = bioReader.SetActiveGym
	// Seed the matcher with whatever gym this sidecar is currently paired
	// to (cached_login row) so the kiosk loop works right after sidecar
	// restart, before any operator re-logs in.
	if gymID := loginGymIDFromCache(uow); gymID != uuid.Nil {
		bioReader.SetActiveGym(gymID)
	}
	authProxy.RegisterRoutes(r)
	authCtrl.RegisterMeRoute(r)
	authCtrl.RegisterAccountRoutes(r)
	authCtrl.RegisterOperatorRoutes(r)
	authCtrl.RegisterUploadsRoute(r)
	authCtrl.RegisterLocalPhotoRoute(r)
	mtCtrl.RegisterRoutes(r)
	promotionsCtrl.RegisterRoutes(r)
	memberCtrl.RegisterRoutes(r)
	fingerprintCtrl.RegisterRoutes(r)
	paymentCtrl.RegisterRoutes(r)
	productCtrl.RegisterRoutes(r)
	expenseController.RegisterRoutes(r)
	checkinCtrl.RegisterRoutes(r)
	kioskCtrl.RegisterRoutes(r)
	biometricCtrl.RegisterRoutes(r)
	notificationsCtrl.RegisterRoutes(r)
	// Ceremony de WhatsApp (UC-037) → proxy al cloud con el sk_live_* del
	// pareo. Owner-only local; el ceremony requiere internet por diseño
	// (misma decisión que checkout). El GET de status quedó arriba, local.
	whatsappProxy := notiCtrl.NewWhatsAppSidecarProxy(notiCtrl.WhatsAppSidecarProxy{
		CloudURL: cloudURL,
		UoW:      uow,
		Tokens:   tokens,
	})
	whatsappProxy.RegisterRoutes(r)
	reportsController.RegisterRoutes(r)
	challengeCtrl.RegisterRoutes(r)
	subscriptionCtrl.RegisterReadOnlyRoutes(r)
	auditCtrl.RegisterRoutes(r)

	// ── Sync agent (Sesión 8 / ADR-001) ──────────────────────────────────
	// The agent reads its sk_live_* sidecar credential from sync_state on
	// boot — the SidecarAuthProxy persisted it during the operator's first
	// online login (ADR-008 §3.3). The agent never sees an operator JWT.
	syncInterval := time.Duration(envInt("SYNC_INTERVAL_S", 30)) * time.Second
	agent := syncShared.NewAgent(syncShared.AgentConfig{
		BaseURL:    cloudURL,
		Interval:   syncInterval,
		Logger:     log.New(os.Stderr, "[sync] ", log.LstdFlags),
		UploadsDir: uploadsDir,
	}, db, uow)
	// Wire the proxy's hooks so a fresh login (or a re-login after the
	// previous credential was revoked) takes effect immediately:
	//   - OnSidecarTokenChanged: hot-swap the agent's in-memory token so
	//     the very next request uses the fresh sk_live_*. Without this,
	//     the agent keeps presenting the revoked token until restart.
	//   - AgentReload: nudge the loop to run RIGHT AWAY rather than waiting
	//     for the 30s tick. Order matters — swap first, then trigger.
	authProxy.OnSidecarTokenChanged = agent.SetToken
	authProxy.AgentReload = agent.TriggerNow
	// Member create/update con foto: empuja al agent inmediatamente
	// para que el upload a R2 ocurra en segundos en lugar de esperar
	// el siguiente tick (hasta 30s).
	memberCtrl.WithSyncTrigger(agent.TriggerNow)
	syncStatusCtrl := syncShared.NewStatusController(agent)
	syncStatusCtrl.RegisterRoutes(r)

	syncCtx, cancelSync := context.WithCancel(context.Background())
	defer cancelSync()
	go agent.Run(syncCtx)

	port := envOrDefault("SIDECAR_PORT", "9090")
	// Bindear ANTES de anunciar. Imprimir LISTENING_ON con el puerto aún
	// sin reservar producía un falso "ready": el SidecarManager de Tauri
	// marca el sidecar como listo al ver la línea, y si el bind fallaba
	// después (típico: huérfano de un update fallido reteniendo 9090) el
	// FE apuntaba durante esa ventana al proceso VIEJO que sí contesta en
	// ese puerto. Con net.Listen primero, un puerto ocupado es un fallo
	// inmediato y limpio (exit → el manager reintenta/reporta) y el
	// anuncio nunca miente.
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		log.Fatalf("bind 127.0.0.1:%s: %v", port, err)
	}
	// ADR-003 §2.2: print the port to stdout so Tauri can capture it.
	fmt.Printf("LISTENING_ON=%s\n", port)
	log.Printf("tinta-sidecar starting on 127.0.0.1:%s db=%s", port, dbPath)
	if err := r.RunListener(ln); err != nil {
		log.Fatalf("gin: %v", err)
	}
}

// loginGymIDFromCache reads gym_id out of the sync_state.cached_login row
// without depending on auth_controller_sidecar's private cachedLoginRow
// shape — we only need the gym_id field. Returns uuid.Nil when the row is
// missing (fresh laptop, redeem not done yet) so callers can render an
// empty-state response instead of erroring out.
func loginGymIDFromCache(uow sharedDomain.UnitOfWork) uuid.UUID {
	var raw string
	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		stx, ok := tx.(*sharedDomain.SqlxTransaction)
		if !ok {
			return errors.New("expected sqlx transaction")
		}
		return stx.Get(context.Background(), &raw,
			`SELECT value FROM sync_state WHERE key = 'cached_login'`)
	})
	if err != nil || raw == "" {
		return uuid.Nil
	}
	var payload struct {
		GymID string `json:"gym_id"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return uuid.Nil
	}
	id, err := uuid.Parse(payload.GymID)
	if err != nil {
		return uuid.Nil
	}
	return id
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

// defaultSidecarDataDir resolves the per-user persistent directory where the
// sidecar keeps its SQLite DB y la cache de uploads cuando SIDECAR_DB_PATH /
// UPLOADS_DIR no vienen seteados. El desktop Tauri normalmente sí los inyecta
// (apuntando al mismo app-data dir del OS); este default es el fallback para
// runs standalone y cualquier caso donde el env no llegue.
//
// Deliberadamente NO usa ./tmp: esa DB es la operación local completa del gym
// (sobrevive reinicios), no scratch data, y un ./tmp relativo además revienta
// cuando el sidecar corre desde un install dir no-escribible (C:\Program
// Files\Tinta). os.UserConfigDir() devuelve %AppData% en Windows,
// ~/Library/Application Support en macOS, ~/.config en Linux.
//
// "app.tinta.desktop" matchea el bundle identifier de Tauri (APP_DIR_NAME en
// cuadra-desktop) — así un run fallback y uno normal aterrizan en el mismo dir.
func defaultSidecarDataDir() string {
	if base, err := os.UserConfigDir(); err == nil {
		return filepath.Join(base, "app.tinta.desktop")
	}
	// os.UserConfigDir sólo falla si HOME/AppData no están seteados —
	// rarísimo. Caemos a un dir claramente persistente bajo el cwd
	// (nunca ./tmp, que se lee como descartable).
	return "tinta-data"
}

// startParentWatcher launches a goroutine that exits the process when
// our parent (the Tauri desktop) is gone. La detección es per-OS
// (parentwatch_unix.go / parentwatch_windows.go):
//
//   - Unix: poll de os.Getppid() cada 2s — al morir el padre nos
//     re-parenta init/launchd y el PPID cambia.
//   - Windows: NO hay reparenting — Getppid devuelve el PID viejo
//     congelado para siempre, así que el poll de PPID nunca detecta
//     nada (bug histórico: el sidecar huérfano vivía indefinido
//     reteniendo el puerto 9090 Y bloqueando tinta-sidecar.exe, que
//     hacía fallar el NSIS del auto-update con "error opening file
//     for writing"). Ahí abrimos un HANDLE al padre y esperamos
//     WaitForSingleObject — señal inmediata y sin polling.
//
// Este watcher es la red de seguridad para crash/process::exit del
// desktop (el updater de Tauri sale así, sin RunEvent::Exit); el camino
// limpio es el shutdown() que el desktop manda al cerrar.
func startParentWatcher() {
	originalParent := os.Getppid()
	if originalParent <= 1 {
		// No real parent (started by init/launchd or daemonised).
		// Likely a CLI invocation we're not supposed to babysit.
		return
	}
	go func() {
		waitForParentExit(originalParent)
		log.Printf("parent process %d gone, shutting down sidecar", originalParent)
		// os.Exit skips defers — nos apoyamos en el WAL recovery de
		// SQLite al siguiente boot. Exit rápido: si colgamos acá, el
		// huérfano retiene el puerto y (en Windows) el .exe que el
		// instalador del update necesita sobreescribir.
		os.Exit(0)
	}()
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

// logRuntimeMode imprime el modo activo al boot. En production guarda
// silencio; en test emite un INFO; en dev grita un WARN. NO hay guardrail
// contra prod-DSN acá: el sidecar corre en la PC del cliente y nunca
// apunta a la base cloud — esa protección vive en cmd/server.
func logRuntimeMode() {
	switch runtime.Current() {
	case runtime.ModeDev:
		log.Printf("WARN ⚠️ TINTA_MODE=dev — Plus gates DISABLED")
	case runtime.ModeTest:
		log.Printf("INFO TINTA_MODE=test — Plus gates respected")
	}
}
