//go:build server

package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	verify "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/verification"
	"github.com/jackc/pgx/v5"
)

func clearLocalPGEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range mancuernaLocalPGVariables {
		t.Setenv(name, "")
	}
}
func syntheticGuardConfig(t *testing.T, c *mancuernaConfig) verify.LocalVerifyGuardConfig {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c.LocalVerifyGuard = &mancuernaLocalVerifyGuard{LedgerPath: filepath.Join(dir, "ledger.json"), Destinations: []string{"+525555559901", "+525555559902"}}
	return verify.LocalVerifyGuardConfig{LedgerPath: c.LocalVerifyGuard.LedgerPath, AccountSID: c.Verify.AccountSID, ServiceSID: c.Verify.ServiceSID, Destinations: c.LocalVerifyGuard.Destinations}
}
func writeLocalConfig(t *testing.T, c mancuernaConfig) string {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMancuernaLocalDSNRejectsRemoteFallbackAndNonTestTargets(t *testing.T) {
	clearLocalPGEnvironment(t)
	commaDirectory := filepath.Join(t.TempDir(), "socket,remote.invalid")
	if err := os.Mkdir(commaDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"", "host=127.0.0.1 dbname=mancuerna_test", "postgresql://runtime@remote.invalid:5432/mancuerna_test",
		"postgresql://runtime@127.0.0.1:5432/production", "postgresql://runtime@127.0.0.1:5432/mancuerna_prod_test",
		"postgresql://runtime@127.0.0.1/mancuerna_test", "postgresql://127.0.0.1:5432/mancuerna_test",
		"postgresql://runtime@127.0.0.1:5432/mancuerna_test?host=remote.invalid",
		"postgresql://runtime@127.0.0.1:5432/mancuerna_test?hostaddr=198.51.100.1",
		"postgresql://runtime@127.0.0.1:5432/mancuerna_test?service=production",
		"postgresql://runtime@127.0.0.1:5432/mancuerna_test?dbname=production",
		"postgresql://runtime@127.0.0.1:5432/mancuerna_test?passfile=/private-file",
		"postgresql://runtime@127.0.0.1:5432/mancuerna_test?sslmode=require",
		"postgresql://runtime@127.0.0.1:5432/mancuerna_test?connect_timeout=0",
		"postgresql://runtime@127.0.0.1:5432/mancuerna_test?host=%2Ftmp&host=remote.invalid",
		"postgresql://runtime@127.0.0.1:5432/mancuerna_test?host=" + url.QueryEscape(commaDirectory),
	} {
		if _, err := localPersonalDSN(raw); err == nil {
			t.Fatal("unsafe local DSN accepted")
		}
	}
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		raw, err := localPersonalDSN("postgresql://runtime:synthetic-password@" + host + ":55449/mancuerna_test")
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := pgx.ParseConfig(raw)
		if err != nil {
			t.Fatal("normalized DSN rejected")
		}
		if parsed.Database != "mancuerna_test" || parsed.Port != 55449 || parsed.User != "runtime" || parsed.Password != "synthetic-password" || parsed.TLSConfig != nil || len(parsed.Fallbacks) != 0 {
			t.Fatal("local DSN changed identity or gained fallback")
		}
		if parsed.Host != "127.0.0.1" && parsed.Host != "::1" {
			t.Fatal("local host still requires DNS")
		}
		if !strings.Contains(raw, "passfile=%2Fdev%2Fnull") {
			t.Fatal("default passfile not disabled")
		}
	}
}

func TestMancuernaLocalRequiresPrivateExplicitGuardWithoutCreatingLedger(t *testing.T) {
	clearLocalPGEnvironment(t)
	t.Setenv("MANCUERNA_CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "forbidden-administrative-database")
	t.Setenv("TWILIO_AUTH_TOKEN", "must-not-be-used")
	if _, _, err := prepareMancuernaLocalOnly(); err == nil {
		t.Fatal("local mode accepted absent config")
	}
	c := syntheticMancuernaConfig(t)
	c.AuthDatabaseURL = "postgresql://runtime_auth@127.0.0.1:55449/mancuerna_test"
	c.DataDatabaseURL = "postgresql://runtime_data@127.0.0.1:55449/mancuerna_test"
	t.Setenv("MANCUERNA_CONFIG_FILE", writeLocalConfig(t, c))
	if _, _, err := prepareMancuernaLocalOnly(); err == nil {
		t.Fatal("local mode accepted missing guard")
	}
	guardConfig := syntheticGuardConfig(t, &c)
	t.Setenv("MANCUERNA_CONFIG_FILE", writeLocalConfig(t, c))
	if _, _, err := prepareMancuernaLocalOnly(); err == nil {
		t.Fatal("startup initialized missing ledger")
	}
	if _, err := os.Stat(guardConfig.LedgerPath); !os.IsNotExist(err) {
		t.Fatal("startup created ledger")
	}
	if _, err := os.Stat(guardConfig.LedgerPath + ".lock"); !os.IsNotExist(err) {
		t.Fatal("startup created ledger lock")
	}
	if err := verify.InitializeLocalVerifyLedger(guardConfig); err != nil {
		t.Fatal(err)
	}
	provider, err := mancuernaProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	guard, ok := provider.(*verify.LocalVerifyGuard)
	if !ok {
		t.Fatal("configured guard omitted in DI")
	}
	status, err := guard.Status(context.Background())
	if err != nil || status.StartsReserved != 0 {
		t.Fatal("DI contacted provider")
	}
}

func TestMancuernaLocalRejectsAmbientPostgresBeforeReadingConfig(t *testing.T) {
	clearLocalPGEnvironment(t)
	t.Setenv("MANCUERNA_CONFIG_FILE", "/nonexistent-private-config")
	for _, name := range mancuernaLocalPGVariables {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "must-not-be-read-or-used")
			if _, _, err := prepareMancuernaLocalOnly(); err == nil || strings.Contains(err.Error(), "must-not-be-read-or-used") {
				t.Fatal("ambient connection was used or revealed")
			}
		})
	}
}

func TestMancuernaLocalModeGatePrecedesDotenvAndRejectsTypos(t *testing.T) {
	for _, value := range []string{"0", "true", " 1", "1 ", "production"} {
		t.Setenv("MANCUERNA_LOCAL_ONLY", value)
		if _, err := mancuernaLocalRequested(); err == nil {
			t.Fatal("ambiguous local mode accepted")
		}
	}
	t.Setenv("MANCUERNA_LOCAL_ONLY", "1")
	if local, err := mancuernaLocalRequested(); err != nil || !local {
		t.Fatal("explicit local mode rejected")
	}
	t.Setenv("MANCUERNA_LOCAL_ONLY", "")
	if local, err := mancuernaLocalRequested(); err != nil || local {
		t.Fatal("local mode implicitly enabled")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var main *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Name.Name == "main" {
			main = f
		}
	}
	if main == nil || len(main.Body.List) < 2 {
		t.Fatal("main startup not found")
	}
	branch, ok := main.Body.List[0].(*ast.IfStmt)
	if !ok {
		t.Fatal("administrative work precedes local branch")
	}
	assignment, ok := branch.Init.(*ast.AssignStmt)
	if !ok || len(assignment.Rhs) != 1 {
		t.Fatal("local mode gate missing")
	}
	call, ok := assignment.Rhs[0].(*ast.CallExpr)
	if !ok {
		t.Fatal("local mode call missing")
	}
	name, ok := call.Fun.(*ast.Ident)
	if !ok || name.Name != "mancuernaLocalRequested" {
		t.Fatal("wrong first startup boundary")
	}
	load, ok := main.Body.List[1].(*ast.AssignStmt)
	if !ok || len(load.Rhs) != 1 {
		t.Fatal("dotenv moved before local boundary")
	}
	call, ok = load.Rhs[0].(*ast.CallExpr)
	if !ok {
		t.Fatal("dotenv call missing")
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Load" {
		t.Fatal("unexpected administrative startup order")
	}
}

func TestMancuernaLocalMainMissingConfigNeverLoadsDotenvOrTinta(t *testing.T) {
	if os.Getenv("MANCUERNA_LOCAL_TEST_CHILD") == "1" {
		main()
		t.Fatal("local startup unexpectedly returned")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, ".env"), []byte("DATABASE_URL=forbidden-dotenv-fixture\nMANCUERNA_CONFIG_FILE=forbidden-relative-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestMancuernaLocalMainMissingConfigNeverLoadsDotenvOrTinta$")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "MANCUERNA_LOCAL_ONLY=1", "MANCUERNA_LOCAL_TEST_CHILD=1"}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("missing config did not fail closed")
	}
	if !strings.Contains(string(out), "No se pudo iniciar la prueba local de Mancuerna") || strings.Contains(string(out), "forbidden-dotenv-fixture") || strings.Contains(string(out), "DATABASE_URL") {
		t.Fatal("local branch reached administrative startup")
	}
}
