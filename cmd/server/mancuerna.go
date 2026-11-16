//go:build server

package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/me"
	phoneVerification "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	hashing "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/budget"
	repos "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/repositories"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	verify "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/verification"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	trainingApp "github.com/cuadra/cuadra-core/src/modules/training/app"
	trainingRepos "github.com/cuadra/cuadra-core/src/modules/training/infraestructure/db/repositories"
	trainingTimezone "github.com/cuadra/cuadra-core/src/modules/training/infraestructure/timezone"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	syncApp "github.com/cuadra/cuadra-core/src/shared/sync/personal/app"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	commandRepos "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/db/repositories"
	cursorCrypto "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/security"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Personal material is loaded only from an explicit private configuration.
// No Tinta database, JWT secret or notification provider is used as a fallback.
type mancuernaKeys struct {
	Active string            `json:"active"`
	Keys   map[string]string `json:"keys"`
}
type mancuernaLimit struct {
	WindowSeconds int `json:"window_seconds"`
	MaxSend       int `json:"max_send"`
	MaxCheck      int `json:"max_check"`
}
type mancuernaRecoveryLimit struct {
	WindowSeconds int `json:"window_seconds"`
	MaxAttempts   int `json:"max_attempts"`
}
type mancuernaLocalVerifyGuard struct {
	LedgerPath   string   `json:"ledger_path"`
	Destinations []string `json:"destinations"`
}

func (mancuernaLocalVerifyGuard) String() string {
	return "local Verify guard (private values redacted)"
}
func (g mancuernaLocalVerifyGuard) GoString() string { return g.String() }

type mancuernaConfig struct {
	MinimumAppVersion   string            `json:"minimum_app_version"`
	PolicyVersion       *string           `json:"policy_version"`
	AuthDatabaseURL     string            `json:"auth_database_url"`
	DataDatabaseURL     string            `json:"data_database_url"`
	JWTKeyID            string            `json:"jwt_key_id"`
	JWTSeed             string            `json:"jwt_seed"`
	JWTVerificationKeys map[string]string `json:"jwt_verification_keys"`
	EnvelopeKeys        mancuernaKeys     `json:"envelope_keys"`
	RecoveryKeys        mancuernaKeys     `json:"recovery_keys"`
	CursorKeys          mancuernaKeys     `json:"cursor_keys"`
	PhoneLockKey        string            `json:"phone_lock_key"`
	BudgetHashKey       string            `json:"budget_hash_key"`
	Verify              struct {
		AccountSID      string `json:"account_sid"`
		ServiceSID      string `json:"service_sid"`
		Username        string `json:"username"`
		Password        string `json:"password"`
		ValiditySeconds int    `json:"validity_seconds"`
		TimeoutSeconds  int    `json:"timeout_seconds"`
	} `json:"verify"`
	Limits                map[budget.Scope]mancuernaLimit `json:"otp_limits"`
	RecoveryLimit         mancuernaRecoveryLimit          `json:"recovery_limit"`
	MaxAttemptChecks      int                             `json:"max_attempt_checks"`
	ResendCooldownSeconds int                             `json:"resend_cooldown_seconds"`
	ReconcileAfterSeconds int                             `json:"reconcile_after_seconds"`
	LocalVerifyGuard      *mancuernaLocalVerifyGuard      `json:"local_verify_guard,omitempty"`
}

func (mancuernaConfig) String() string     { return "Mancuerna configuration (secrets redacted)" }
func (c mancuernaConfig) GoString() string { return c.String() }

var errMancuernaConfig = errors.New("configuración privada de Mancuerna inválida; revisa el archivo y las migraciones personales")

func readMancuernaConfig(path string) (mancuernaConfig, error) {
	var c mancuernaConfig
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64*1024 {
		return c, errMancuernaConfig
	}
	f, err := os.Open(path)
	if err != nil {
		return c, errMancuernaConfig
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 64*1024+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil {
		return mancuernaConfig{}, errMancuernaConfig
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return mancuernaConfig{}, errMancuernaConfig
	}
	return c, nil
}
func personalKey(encoded string) ([]byte, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(b) != 32 || len(encoded) != 43 {
		return nil, errMancuernaConfig
	}
	return b, nil
}
func personalKeyRing(c mancuernaKeys) (map[string][]byte, error) {
	keys := make(map[string][]byte, len(c.Keys))
	for id, encoded := range c.Keys {
		key, err := personalKey(encoded)
		if err != nil {
			return nil, err
		}
		keys[id] = key
	}
	return keys, nil
}
func openPersonalPool(dsn string, role shared.PersonalRole) (*gorm.DB, shared.UnitOfWork, func(), error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, nil, nil, errMancuernaConfig
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard, DisableAutomaticPing: true})
	if err != nil {
		return nil, nil, nil, errMancuernaConfig
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, nil, errMancuernaConfig
	}
	closePool := func() { _ = sqlDB.Close() }
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	// Revision 6 preserves client capture metadata independently of commit time.
	var version int
	if db.Raw("SELECT COALESCE(max(version),0) FROM mancuerna.schema_versions").Scan(&version).Error != nil || version < 6 {
		closePool()
		return nil, nil, nil, errMancuernaConfig
	}
	uow, err := shared.NewPersonalPostgresUnitOfWork(db, role)
	if err != nil {
		closePool()
		return nil, nil, nil, errMancuernaConfig
	}
	return db, uow, closePool, nil
}

// Configuration being absent leaves the existing Tinta server unchanged.
// This function never migrates, grants privileges or creates signing material.
func configureMancuerna(router *gin.Engine) (func(), error) {
	path := os.Getenv("MANCUERNA_CONFIG_FILE")
	if path == "" {
		return func() {}, nil
	}
	c, err := readMancuernaConfig(path)
	if err != nil {
		return nil, err
	}
	return mountMancuerna(router, c)
}
func mountMancuerna(router *gin.Engine, c mancuernaConfig) (cleanup func(), err error) {
	if router == nil || c.AuthDatabaseURL == "" || c.DataDatabaseURL == "" || c.AuthDatabaseURL == c.DataDatabaseURL {
		return nil, errMancuernaConfig
	}
	seed, err := personalKey(c.JWTSeed)
	if err != nil {
		return nil, err
	}
	publics := map[string]ed25519.PublicKey{}
	for id, value := range c.JWTVerificationKeys {
		key, err := personalKey(value)
		if err != nil {
			return nil, err
		}
		publics[id] = ed25519.PublicKey(key)
	}
	tokens, err := crypto.NewAccessTokens(c.JWTKeyID, ed25519.NewKeyFromSeed(seed), publics)
	if err != nil {
		return nil, errMancuernaConfig
	}
	envelopeMaterial, err := personalKeyRing(c.EnvelopeKeys)
	if err != nil {
		return nil, err
	}
	envelopes, err := crypto.NewEnvelopes(c.EnvelopeKeys.Active, envelopeMaterial)
	if err != nil {
		return nil, errMancuernaConfig
	}
	recoveryMaterial, err := personalKeyRing(c.RecoveryKeys)
	if err != nil {
		return nil, err
	}
	recoveryKeys, err := crypto.NewRecoveryKeys(c.RecoveryKeys.Active, recoveryMaterial)
	if err != nil {
		return nil, errMancuernaConfig
	}
	cursorMaterial, err := personalKeyRing(c.CursorKeys)
	if err != nil {
		return nil, err
	}
	cursors, err := cursorCrypto.NewCursorSigner(c.CursorKeys.Active, cursorMaterial)
	if err != nil {
		return nil, errMancuernaConfig
	}
	phoneKey, err := personalKey(c.PhoneLockKey)
	if err != nil {
		return nil, err
	}
	budgetKey, err := personalKey(c.BudgetHashKey)
	if err != nil {
		return nil, err
	}
	// The six purposes must not share current secret material.
	materials := [][]byte{seed, phoneKey, budgetKey, envelopeMaterial[c.EnvelopeKeys.Active], recoveryMaterial[c.RecoveryKeys.Active], cursorMaterial[c.CursorKeys.Active]}
	for i := range materials {
		for j := 0; j < i; j++ {
			if bytes.Equal(materials[i], materials[j]) {
				return nil, errMancuernaConfig
			}
		}
	}
	identity, err := repos.NewIdentityAccessRepository(phoneKey)
	if err != nil {
		return nil, errMancuernaConfig
	}
	subjects, err := hashing.NewSubjects(budgetKey)
	if err != nil {
		return nil, errMancuernaConfig
	}
	limits := map[budget.Scope]budget.Limit{}
	for scope, limit := range c.Limits {
		if limit.WindowSeconds < 1 || limit.WindowSeconds > 86400*366 {
			return nil, errMancuernaConfig
		}
		limits[scope] = budget.Limit{Window: time.Duration(limit.WindowSeconds) * time.Second, MaxSend: limit.MaxSend, MaxCheck: limit.MaxCheck}
	}
	rules, err := budget.NewRules(limits)
	if err != nil {
		return nil, errMancuernaConfig
	}
	if c.Verify.ValiditySeconds < 120 || c.Verify.ValiditySeconds > 86400 || c.Verify.TimeoutSeconds < 1 || c.Verify.TimeoutSeconds > 30 || c.ReconcileAfterSeconds < c.Verify.TimeoutSeconds || c.ReconcileAfterSeconds > 3600 || c.ResendCooldownSeconds < 1 || c.ResendCooldownSeconds > 3600 || c.MaxAttemptChecks < 1 || c.MaxAttemptChecks > 2147483647 {
		return nil, errMancuernaConfig
	}
	if c.RecoveryLimit.WindowSeconds < 1 || c.RecoveryLimit.WindowSeconds > 86400*366 || c.RecoveryLimit.MaxAttempts < 1 || c.RecoveryLimit.MaxAttempts > 2147483647 {
		return nil, errMancuernaConfig
	}
	recoveryRules, err := budget.NewRecoveryRules(budget.Limit{Window: time.Duration(c.RecoveryLimit.WindowSeconds) * time.Second, MaxSend: 1, MaxCheck: c.RecoveryLimit.MaxAttempts})
	if err != nil {
		return nil, errMancuernaConfig
	}
	provider, err := mancuernaProvider(c)
	if err != nil {
		return nil, errMancuernaConfig
	}
	authDB, authUoW, closeAuth, err := openPersonalPool(c.AuthDatabaseURL, shared.PersonalAuthRole)
	if err != nil {
		return nil, err
	}
	_, dataUoW, closeData, err := openPersonalPool(c.DataDatabaseURL, shared.PersonalDataRole)
	if err != nil {
		closeAuth()
		return nil, err
	}
	closePools := func() { closeData(); closeAuth() }
	cleanup = closePools
	defer func() {
		if err != nil {
			closePools()
		}
	}()
	attempts := repos.NewAuthAttemptPostgresRepository()
	receipts := repos.NewAuthReceiptPostgresRepository()
	sessions := repos.NewAuthSessionRepository()
	recovery := repos.NewRecoveryKeyRepository()
	events := repos.NewSecurityEventRepository()
	d := personalApp.StartAuthDependencies{UoW: authUoW, Attempts: attempts, Receipts: receipts, Budgets: repos.NewOTPBudgetRepository(), Rules: rules, Subjects: subjects, Provider: provider, ServiceIdentity: c.Verify.ServiceSID, ProviderValidity: time.Duration(c.Verify.ValiditySeconds) * time.Second, Now: time.Now}
	start, err := personalApp.NewStartAuth(d)
	if err != nil {
		return nil, errMancuernaConfig
	}
	check, err := personalApp.NewCheckPhoneVerification(d, c.MaxAttemptChecks)
	if err != nil {
		return nil, errMancuernaConfig
	}
	finalize, err := personalApp.NewFinalizeAccess(personalApp.FinalizeAccessDependencies{UoW: authUoW, Attempts: attempts, Identity: identity, Sessions: sessions, Recovery: recovery, Receipts: receipts, Events: events, Tokens: tokens, Proofs: crypto.DeviceProofs{}, RecoverySecrets: recoveryKeys, Envelopes: envelopes, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	checkAccess, err := personalApp.NewCheckAccess(check, finalize)
	if err != nil {
		return nil, errMancuernaConfig
	}
	retry := personalApp.PhoneRetryPolicy{ProviderTimeout: time.Duration(c.Verify.TimeoutSeconds) * time.Second, ReconcileAfter: time.Duration(c.ReconcileAfterSeconds) * time.Second, ResendCooldown: time.Duration(c.ResendCooldownSeconds) * time.Second}
	get, err := personalApp.NewGetAttempt(personalApp.GetAttemptDependencies{UoW: authUoW, Attempts: attempts, Provider: provider, Now: time.Now, ProviderTimeout: retry.ProviderTimeout, ReconcileAfter: retry.ReconcileAfter})
	if err != nil {
		return nil, errMancuernaConfig
	}
	authorizations := repos.NewDeviceAuthorizationRepository()
	authorizationStatus, err := personalApp.NewGetDeviceAuthorizationStatus(personalApp.GetDeviceAuthorizationStatusDependencies{UoW: authUoW, Attempts: attempts, Authorizations: authorizations, Sessions: sessions, Receipts: receipts, Envelopes: envelopes, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	recoveryStatus, err := personalApp.NewGetRecoveryStatus(personalApp.GetRecoveryStatusDependencies{UoW: authUoW, Attempts: attempts, Recovery: recovery, Sessions: sessions, Receipts: receipts, RecoverySecrets: recoveryKeys, Envelopes: envelopes, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	status, err := personalApp.NewGetAuthStatus(get, finalize, authorizationStatus, recoveryStatus)
	if err != nil {
		return nil, errMancuernaConfig
	}
	resend, err := personalApp.NewResendAuth(d, retry)
	if err != nil {
		return nil, errMancuernaConfig
	}
	// UI uses the same effective cooldown as the resend command.
	cooldown := retry.ResendCooldown
	if cooldown < retry.ReconcileAfter {
		cooldown = retry.ReconcileAfter
	}
	startHTTP, err := controllers.NewStartAuthController(start, time.Now, cooldown)
	if err != nil {
		return nil, errMancuernaConfig
	}
	capabilities := me.Capabilities{ProtocolVersion: 1, MinimumProtocolVersion: 1, MinimumAppVersion: c.MinimumAppVersion, MaxPushOperations: 50, MaxRequestBytes: 2097152, MaxGroupBytes: 1048576, Features: []string{}, PolicyVersion: c.PolicyVersion}
	getMe, err := personalApp.NewGetMe(personalApp.GetMeDependencies{UoW: authUoW, Sessions: sessions, Me: repos.NewMeRepository(), Capabilities: capabilities})
	if err != nil {
		return nil, errMancuernaConfig
	}
	confirm, err := personalApp.NewConfirmRecoveryKey(personalApp.ConfirmRecoveryKeyDependencies{UoW: authUoW, Sessions: sessions, Recovery: recovery, Receipts: receipts, Events: events, Envelopes: envelopes, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	meHTTP, err := controllers.NewMeController(getMe, confirm)
	if err != nil {
		return nil, errMancuernaConfig
	}
	authenticate, err := personalMiddleware.NewPersonal(tokens, time.Now)
	if err != nil {
		return nil, errMancuernaConfig
	}
	refreshIdentity, err := repos.NewRefreshIdentityResolver(authDB)
	if err != nil {
		return nil, errMancuernaConfig
	}
	refreshSession, err := personalApp.NewRefreshSession(personalApp.RefreshSessionDependencies{UoW: authUoW, Identity: refreshIdentity, Rotations: repos.NewRefreshSessionRepository(), Sessions: sessions, Events: events, Tokens: tokens, Proofs: crypto.DeviceProofs{}, Envelopes: envelopes, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	logoutRepo, err := repos.NewLogoutRepository(authDB)
	if err != nil {
		return nil, errMancuernaConfig
	}
	logout, err := personalApp.NewLogout(personalApp.LogoutDependencies{UoW: authUoW, Logout: logoutRepo, Sessions: sessions, Receipts: receipts, Events: events, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	sessionHTTP, err := controllers.NewSessionController(refreshSession, logout)
	if err != nil {
		return nil, errMancuernaConfig
	}
	proofs := repos.NewDeviceProofRepository()
	startProof, err := personalApp.NewStartDeviceProof(personalApp.StartDeviceProofDependencies{UoW: authUoW, Sessions: sessions, Proofs: proofs, Receipts: receipts, Events: events, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	completeProof, err := personalApp.NewCompleteDeviceProof(personalApp.CompleteDeviceProofDependencies{UoW: authUoW, Sessions: sessions, Proofs: proofs, Receipts: receipts, Events: events, Verifier: crypto.DeviceProofs{}, Envelopes: envelopes, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	proofHTTP, err := controllers.NewDeviceProofController(startProof, completeProof)
	if err != nil {
		return nil, errMancuernaConfig
	}

	rotateKit, err := personalApp.NewRotateRecoveryKey(personalApp.RotateRecoveryKeyDependencies{UoW: authUoW, Sessions: sessions, Recovery: recovery, Proofs: proofs, Receipts: receipts, Events: events, RecoverySecrets: recoveryKeys, Envelopes: envelopes, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	recoveryHTTP, err := controllers.NewRecoveryKeyController(rotateKit)
	if err != nil {
		return nil, errMancuernaConfig
	}
	updateProfile, err := personalApp.NewUpdateProfile(personalApp.UpdateProfileDependencies{UoW: dataUoW, Accounts: repos.NewAccountPostgresRepository(), Commands: commandRepos.NewCommands(), Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	createExercise, err := trainingApp.NewCreatePersonalExercise(trainingApp.CreatePersonalExerciseDependencies{UoW: dataUoW, Exercises: trainingRepos.NewPersonalExercisePostgresRepository(), Commands: commandRepos.NewCommands(), Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	createRoutine, err := trainingApp.NewCreateRoutine(trainingApp.CreateRoutineDependencies{UoW: dataUoW, Routines: trainingRepos.NewRoutinePostgresRepository(), Commands: commandRepos.NewCommands(), Stream: commandRepos.NewStream(), Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	saveDraft, err := trainingApp.NewSaveRoutineDraft(trainingApp.SaveRoutineDraftDependencies{UoW: dataUoW, Routines: trainingRepos.NewRoutinePostgresRepository(), Drafts: trainingRepos.NewRoutinePostgresRepository(), Commands: commandRepos.NewCommands(), Stream: commandRepos.NewStream(), Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	publishRoutine, err := trainingApp.NewPublishRoutine(trainingApp.PublishRoutineDependencies{UoW: dataUoW, Routines: trainingRepos.NewRoutinePostgresRepository(), Publications: trainingRepos.NewRoutinePublicationPostgresRepository(), Commands: commandRepos.NewCommands(), Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	commandHTTP, err := controllers.NewCommandControllerWithDependencies(controllers.CommandDependencies{Profile: updateProfile, Exercise: createExercise, Routine: createRoutine, Draft: saveDraft, Publish: publishRoutine})
	if err != nil {
		return nil, errMancuernaConfig
	}
	listExercises, err := trainingApp.NewListExercises(trainingApp.ListExercisesDependencies{UoW: dataUoW, Commands: commandRepos.NewCommands(), Stream: commandRepos.NewStream(), Exercises: trainingRepos.NewExerciseListPostgresRepository(), Cursors: cursors, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	exercisesHTTP, err := controllers.NewExercisesController(listExercises)
	if err != nil {
		return nil, errMancuernaConfig
	}
	routineReads := trainingApp.RoutineReadDependencies{UoW: dataUoW, Commands: commandRepos.NewCommands(), Routines: trainingRepos.NewRoutinePostgresRepository(), Publications: trainingRepos.NewRoutinePublicationPostgresRepository()}
	listRoutines, err := trainingApp.NewListRoutines(trainingApp.ListRoutinesDependencies{UoW: dataUoW, Commands: routineReads.Commands, Stream: commandRepos.NewStream(), Routines: trainingRepos.NewRoutinePostgresRepository(), Cursors: cursors, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	getRoutine, err := trainingApp.NewGetRoutine(routineReads)
	if err != nil {
		return nil, errMancuernaConfig
	}
	getRoutineDraft, err := trainingApp.NewGetRoutineDraft(routineReads)
	if err != nil {
		return nil, errMancuernaConfig
	}
	getRoutineVersion, err := trainingApp.NewGetRoutineVersion(routineReads)
	if err != nil {
		return nil, errMancuernaConfig
	}
	routinesHTTP, err := controllers.NewRoutinesController(listRoutines, getRoutine, getRoutineDraft, getRoutineVersion)
	if err != nil {
		return nil, errMancuernaConfig
	}

	startWorkout, err := trainingApp.NewStartWorkout(trainingApp.StartWorkoutDependencies{UoW: dataUoW, Versions: trainingRepos.NewRoutinePublicationPostgresRepository(), Workouts: trainingRepos.NewWorkoutPostgresRepository(), Commands: commandRepos.NewCommands(), Canonicalizer: crypto.JSONCanonicalizer{}, Timezones: trainingTimezone.NewIANA(), Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	createSet, err := trainingApp.NewCreateWorkoutSet(trainingApp.CreateWorkoutSetDependencies{UoW: dataUoW, Workouts: trainingRepos.NewWorkoutPostgresRepository(), Sets: trainingRepos.NewWorkoutSetPostgresRepository(), Commands: commandRepos.NewCommands(), Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	pushWorkouts, err := trainingApp.NewPushWorkouts(trainingApp.PushWorkoutsDependencies{UoW: dataUoW, Commands: commandRepos.NewCommands(), Start: startWorkout, CreateSet: createSet, Canonicalizer: crypto.JSONCanonicalizer{}})
	if err != nil {
		return nil, errMancuernaConfig
	}
	pushHTTP, err := controllers.NewPushController(pushWorkouts)
	if err != nil {
		return nil, errMancuernaConfig
	}

	vaults := repos.NewVaultRepository()
	createVault, err := personalApp.NewCreatePersonalVault(personalApp.CreatePersonalVaultDependencies{UoW: authUoW, Sessions: sessions, Vaults: vaults, Receipts: receipts, Events: events, Envelopes: envelopes, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	unlockVault, err := personalApp.NewUnlockPersonalVault(personalApp.UnlockPersonalVaultDependencies{UoW: authUoW, Sessions: sessions, Vaults: vaults, Proofs: proofs, Receipts: receipts, Events: events, Envelopes: envelopes, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	vaultHTTP, err := controllers.NewVaultController(createVault, unlockVault)
	if err != nil {
		return nil, errMancuernaConfig
	}
	pull, err := syncApp.NewPull(syncApp.PullDependencies{UoW: dataUoW, Commands: commandRepos.NewCommands(), Stream: commandRepos.NewStream(), Cursors: cursors, Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	pullHTTP, err := controllers.NewSyncPullController(pull)
	if err != nil {
		return nil, errMancuernaConfig
	}
	devices := repos.NewDevicesRepository()
	listDevices, err := personalApp.NewListDevices(personalApp.ListDevicesDependencies{UoW: authUoW, Sessions: sessions, Devices: devices, Cursors: cursors, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	revokeDevices, err := personalApp.NewRevokeDevices(personalApp.RevokeDevicesDependencies{UoW: authUoW, Sessions: sessions, Devices: devices, Proofs: proofs, Receipts: receipts, Events: events, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	devicesHTTP, err := controllers.NewDevicesController(listDevices, revokeDevices)
	if err != nil {
		return nil, errMancuernaConfig
	}
	startAuthorization, err := personalApp.NewStartDeviceAuthorization(personalApp.StartDeviceAuthorizationDependencies{UoW: authUoW, Attempts: attempts, Authorizations: authorizations, Receipts: receipts, Events: events, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	listAuthorizations, err := personalApp.NewListDeviceAuthorizations(personalApp.ListDeviceAuthorizationsDependencies{UoW: authUoW, Sessions: sessions, Authorizations: authorizations, Cursors: cursors, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	decideAuthorization, err := personalApp.NewDecideDeviceAuthorization(personalApp.DecideDeviceAuthorizationDependencies{UoW: authUoW, Attempts: attempts, Authorizations: authorizations, Sessions: sessions, Proofs: proofs, Receipts: receipts, Events: events, Tokens: tokens, Envelopes: envelopes, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	authorizationHTTP, err := controllers.NewDeviceAuthorizationController(startAuthorization, listAuthorizations, decideAuthorization)
	if err != nil {
		return nil, errMancuernaConfig
	}
	getAuthorizationResult, err := personalApp.NewGetDeviceAuthorizationDecisionResult(personalApp.GetDeviceAuthorizationDecisionResultDependencies{UoW: authUoW, Sessions: sessions, Receipts: receipts, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	authorizationResultHTTP, err := controllers.NewDeviceAuthorizationResultController(getAuthorizationResult)
	if err != nil {
		return nil, errMancuernaConfig
	}

	snapshotUoW, ok := dataUoW.(shared.PersonalSnapshotUnitOfWork)
	if !ok {
		return nil, errMancuernaConfig
	}
	snapshots, err := commandRepos.NewSnapshots(crypto.JSONCanonicalizer{})
	if err != nil {
		return nil, errMancuernaConfig
	}
	snapshotCapabilities := protocol.Capabilities{ProtocolVersion: capabilities.ProtocolVersion, MinimumProtocolVersion: capabilities.MinimumProtocolVersion, MinimumAppVersion: capabilities.MinimumAppVersion, MaxPushOperations: capabilities.MaxPushOperations, MaxRequestBytes: capabilities.MaxRequestBytes, MaxGroupBytes: capabilities.MaxGroupBytes, Features: capabilities.Features, PolicyVersion: capabilities.PolicyVersion}
	createSnapshot, err := syncApp.NewCreateSnapshot(syncApp.CreateSnapshotDependencies{UoW: snapshotUoW, Snapshots: snapshots, Cursors: cursors, Capabilities: snapshotCapabilities, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	getSnapshotPage, err := syncApp.NewGetSnapshotPage(syncApp.GetSnapshotPageDependencies{UoW: dataUoW, Snapshots: snapshots, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	snapshotHTTP, err := controllers.NewSyncSnapshotController(createSnapshot, getSnapshotPage)
	if err != nil {
		return nil, errMancuernaConfig
	}
	recoveryIdentity, err := repos.NewRecoveryIdentityRepository(phoneKey)
	if err != nil {
		return nil, errMancuernaConfig
	}
	completeRecovery, err := personalApp.NewCompleteRecovery(personalApp.CompleteRecoveryDependencies{UoW: authUoW, Attempts: attempts, Identity: recoveryIdentity, Recovery: recovery, Sessions: sessions, Receipts: receipts, Events: events, Tokens: tokens, RecoverySecrets: recoveryKeys, Envelopes: envelopes, Budgets: repos.NewOTPBudgetRepository(), Rules: recoveryRules, Subjects: subjects, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	completeRecoveryHTTP, err := controllers.NewRecoveryController(completeRecovery)
	if err != nil {
		return nil, errMancuernaConfig
	}
	phoneChangeIdentity, err := repos.NewPhoneChangeRepository(phoneKey)
	if err != nil {
		return nil, errMancuernaConfig
	}
	phoneChangeDependencies := personalApp.PhoneChangeDependencies{Phone: d, Identity: phoneChangeIdentity, Sessions: sessions, Proofs: proofs, Events: events, Tokens: tokens, Envelopes: envelopes, MaxChecks: c.MaxAttemptChecks}
	startPhoneChange, err := personalApp.NewStartPhoneChange(phoneChangeDependencies)
	if err != nil {
		return nil, errMancuernaConfig
	}
	completePhoneChange, err := personalApp.NewCompletePhoneChange(phoneChangeDependencies)
	if err != nil {
		return nil, errMancuernaConfig
	}
	phoneChangeHTTP, err := controllers.NewPhoneChangeController(startPhoneChange, completePhoneChange, time.Now, cooldown)
	if err != nil {
		return nil, errMancuernaConfig
	}
	phoneAttemptDependencies := personalApp.PhoneChangeAttemptDependencies{Phone: phoneChangeDependencies, Resolver: repos.NewPhoneChangeAttemptResolver(), Policy: retry}
	phoneAttempt, err := personalApp.NewGetPhoneChangeAttempt(phoneAttemptDependencies)
	if err != nil {
		return nil, errMancuernaConfig
	}
	phoneResend, err := personalApp.NewResendPhoneChange(phoneAttemptDependencies)
	if err != nil {
		return nil, errMancuernaConfig
	}
	statusRouter, err := personalApp.NewGetAuthStatusRouter(authUoW, attempts, status, phoneAttempt)
	if err != nil {
		return nil, errMancuernaConfig
	}
	resendRouter, err := personalApp.NewResendAuthRouter(authUoW, attempts, resend, phoneResend)
	if err != nil {
		return nil, errMancuernaConfig
	}
	accessHTTP, err := controllers.NewAccessController(checkAccess, statusRouter, resendRouter, time.Now, cooldown)
	if err != nil {
		return nil, errMancuernaConfig
	}
	getPersonalResult, err := personalApp.NewGetPersonalAuthResult(personalApp.GetPersonalAuthResultDependencies{UoW: authUoW, Sessions: sessions, Results: repos.NewPersonalAuthResultRepository(), Recovery: recovery, RecoverySecrets: recoveryKeys, Envelopes: envelopes, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	deletionRepository := repos.NewDeletionRepository()
	deletionResolver, err := repos.NewDeletionResultResolver(authDB)
	if err != nil {
		return nil, errMancuernaConfig
	}
	getDeletionResult, err := personalApp.NewGetDeletionResult(personalApp.GetDeletionResultDependencies{UoW: authUoW, Resolver: deletionResolver, Deletion: deletionRepository})
	if err != nil {
		return nil, errMancuernaConfig
	}
	// Retention/restore policy has not been approved. Mount a fail-closed
	// operation; only explicitly configured test use cases can accept requests.
	requestDeletion, err := personalApp.NewRequestAccountDeletion(personalApp.RequestAccountDeletionDependencies{UoW: authUoW, Sessions: sessions, Deletion: deletionRepository, Proofs: proofs, Receipts: receipts, Events: events, Policy: nil, Now: time.Now})
	if err != nil {
		return nil, errMancuernaConfig
	}
	deletionHTTP, err := controllers.NewDeletionController(requestDeletion)
	if err != nil {
		return nil, errMancuernaConfig
	}
	resultAccess, err := personalMiddleware.NewResultAccess(authenticate)
	if err != nil {
		return nil, errMancuernaConfig
	}
	resultHTTP, err := controllers.NewAuthResultControllerWithDeletion(getPersonalResult, getDeletionResult)
	if err != nil {
		return nil, errMancuernaConfig
	}
	group := router.Group("/api/mancuerna/v1")
	group.POST("/auth/start", startHTTP.Start)
	group.POST("/auth/recovery/complete", completeRecoveryHTTP.Complete)
	group.POST("/auth/check", accessHTTP.Check)
	group.GET("/auth/attempts/:id", accessHTTP.Get)
	group.POST("/auth/attempts/:id/resend", accessHTTP.Resend)
	group.POST("/auth/refresh", sessionHTTP.Refresh)
	group.POST("/auth/logout", sessionHTTP.Logout)
	group.GET("/auth/results/:id", resultAccess, resultHTTP.Get)
	group.GET("/me", authenticate, meHTTP.Get)
	group.POST("/me/recovery-key/confirm", authenticate, meHTTP.ConfirmRecovery)
	group.POST("/me/proofs", authenticate, proofHTTP.Start)
	group.POST("/me/proofs/:id/complete", authenticate, proofHTTP.Complete)
	group.POST("/me/recovery-key", authenticate, recoveryHTTP.Rotate)
	group.POST("/me/phone-change/start", authenticate, phoneChangeHTTP.Start)
	group.POST("/me/phone-change/complete", authenticate, phoneChangeHTTP.Complete)
	group.POST("/me/deletion", authenticate, deletionHTTP.Request)
	group.POST("/commands", authenticate, commandHTTP.Execute)
	group.GET("/exercises", authenticate, exercisesHTTP.List)
	group.GET("/routines", authenticate, routinesHTTP.List)
	group.GET("/routines/:id", authenticate, routinesHTTP.Get)
	group.GET("/routines/:id/draft", authenticate, routinesHTTP.Draft)
	group.GET("/routine-versions/:id", authenticate, routinesHTTP.Version)
	group.POST("/sync/push", authenticate, pushHTTP.Execute)
	group.POST("/me/vaults", authenticate, vaultHTTP.Create)
	group.POST("/me/vaults/:id/unlock", authenticate, vaultHTTP.Unlock)
	group.GET("/sync/pull", authenticate, pullHTTP.Get)
	group.GET("/me/devices", authenticate, devicesHTTP.List)
	group.POST("/me/devices/:id/revoke", authenticate, devicesHTTP.RevokeOne)
	group.POST("/me/devices/revoke", authenticate, devicesHTTP.RevokeScope)
	group.POST("/auth/device-authorizations", authorizationHTTP.Start)
	group.GET("/me/device-authorizations", authenticate, authorizationHTTP.List)
	group.POST("/me/device-authorizations/:id/decision", authenticate, authorizationHTTP.Decide)
	group.GET("/me/device-authorizations/:id/decisions/:operation_id", authenticate, authorizationResultHTTP.Get)
	group.POST("/sync/snapshots", authenticate, snapshotHTTP.Create)
	group.GET("/sync/snapshots/:id/pages/:number", authenticate, snapshotHTTP.Page)
	return cleanup, nil
}

func mancuernaProvider(c mancuernaConfig) (phoneVerification.VerificationProvider, error) {
	provider, err := verify.NewTwilio(verify.TwilioConfig{AccountSID: c.Verify.AccountSID, ServiceSID: c.Verify.ServiceSID, Username: c.Verify.Username, Password: c.Verify.Password, ServiceValidity: time.Duration(c.Verify.ValiditySeconds) * time.Second, RequestTimeout: time.Duration(c.Verify.TimeoutSeconds) * time.Second})
	if err != nil {
		return nil, errMancuernaConfig
	}
	if c.LocalVerifyGuard == nil {
		return provider, nil
	}
	guard, err := verify.NewLocalVerifyGuard(verify.LocalVerifyGuardConfig{LedgerPath: c.LocalVerifyGuard.LedgerPath, AccountSID: c.Verify.AccountSID, ServiceSID: c.Verify.ServiceSID, Destinations: c.LocalVerifyGuard.Destinations}, provider)
	if err != nil {
		return nil, errMancuernaConfig
	}
	return guard, nil
}
