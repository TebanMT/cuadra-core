//go:build server && (darwin || linux)

package verification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pe "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	phone "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type localGuardProviderDouble struct {
	starts, checks, fetches atomic.Int32
	start                   func(context.Context, phone.StartRequest) (phone.Result, error)
}

func (p *localGuardProviderDouble) Start(ctx context.Context, in phone.StartRequest) (phone.Result, error) {
	p.starts.Add(1)
	if p.start != nil {
		return p.start(ctx, in)
	}
	now := time.Now().UTC()
	return phone.Result{Reference: "VE" + strings.ReplaceAll(uuid.NewString(), "-", ""), Channel: in.Channel, State: phone.Pending, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}, nil
}
func (p *localGuardProviderDouble) Check(_ context.Context, in phone.CheckRequest) (phone.Result, error) {
	p.checks.Add(1)
	return phone.Result{Reference: in.Reference, State: phone.Approved}, nil
}
func (p *localGuardProviderDouble) Fetch(_ context.Context, in phone.Binding) (phone.Result, error) {
	p.fetches.Add(1)
	return phone.Result{Reference: in.Reference, State: phone.Pending}, nil
}

func localGuardConfig(t *testing.T) LocalVerifyGuardConfig {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return LocalVerifyGuardConfig{LedgerPath: filepath.Join(dir, "private-ledger.json"), AccountSID: "AC" + strings.Repeat("1", 32), ServiceSID: "VA" + strings.Repeat("2", 32), Destinations: []string{"+525555550001", "+525555550002"}}
}
func newLocalGuard(t *testing.T, c LocalVerifyGuardConfig, p phone.VerificationProvider) *LocalVerifyGuard {
	t.Helper()
	g, err := newLocalVerifyGuard(c, p)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func localGuardSetup(t *testing.T) (LocalVerifyGuardConfig, *LocalVerifyGuard, *localGuardProviderDouble) {
	t.Helper()
	c := localGuardConfig(t)
	if err := InitializeLocalVerifyLedger(c); err != nil {
		t.Fatal(err)
	}
	p := &localGuardProviderDouble{}
	return c, newLocalGuard(t, c, p), p
}
func localStart(c LocalVerifyGuardConfig) phone.StartRequest {
	return phone.StartRequest{DestinationE164: c.Destinations[0], Channel: phone.SMS}
}

func TestLocalVerifyGuardRequiresExplicitInitializationAndPinsConfiguration(t *testing.T) {
	c := localGuardConfig(t)
	p := &localGuardProviderDouble{}
	if _, err := newLocalVerifyGuard(c, p); !errors.Is(err, pe.ErrVerificationConfiguration) {
		t.Fatal("missing ledger accepted", err)
	}
	if _, err := os.Stat(c.LedgerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("constructor created ledger")
	}
	if err := InitializeLocalVerifyLedger(c); err != nil {
		t.Fatal(err)
	}
	if err := InitializeLocalVerifyLedger(c); !errors.Is(err, pe.ErrVerificationConfiguration) {
		t.Fatal("initialization reset ledger", err)
	}
	for name, change := range map[string]func(*LocalVerifyGuardConfig){
		"service":               func(c *LocalVerifyGuardConfig) { c.ServiceSID = "VA" + strings.Repeat("3", 32) },
		"account":               func(c *LocalVerifyGuardConfig) { c.AccountSID = "AC" + strings.Repeat("3", 32) },
		"allowlist":             func(c *LocalVerifyGuardConfig) { c.Destinations = []string{"+525555550001", "+525555550003"} },
		"duplicate destination": func(c *LocalVerifyGuardConfig) { c.Destinations = []string{"+525555550001", "+525555550001"} },
		"one destination":       func(c *LocalVerifyGuardConfig) { c.Destinations = []string{"+525555550001"} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := c
			change(&changed)
			if _, err := newLocalVerifyGuard(changed, p); !errors.Is(err, pe.ErrVerificationConfiguration) {
				t.Fatal("changed private binding accepted", err)
			}
		})
	}
	if p.starts.Load()+p.checks.Load()+p.fetches.Load() != 0 {
		t.Fatal("configuration contacted provider")
	}
}

func TestLocalVerifyGuardReservesBeforeSendNeverRefundsAndSurvivesRestart(t *testing.T) {
	c, g, p := localGuardSetup(t)
	p.start = func(context.Context, phone.StartRequest) (phone.Result, error) {
		// Constructing another guard here proves no lock spans the provider.
		other := newLocalGuard(t, c, p)
		status, err := other.Status(context.Background())
		if err != nil || status.StartsReserved != int(p.starts.Load()) {
			t.Fatal("reservation not durable before send", err)
		}
		return phone.Result{}, failure(phone.Uncertain)
	}
	for i := 0; i < LocalVerifyMaximumStarts; i++ {
		g = newLocalGuard(t, c, p)
		in := localStart(c)
		if i%2 == 1 {
			in.DestinationE164 = c.Destinations[1]
			in.Channel = phone.WhatsApp
		}
		if _, err := g.Start(context.Background(), in); !errors.Is(err, pe.ErrVerificationUncertain) {
			t.Fatal(err)
		}
	}
	if _, err := newLocalGuard(t, c, p).Start(context.Background(), localStart(c)); !errors.Is(err, pe.ErrVerificationRateLimited) {
		t.Fatal("fifth send allowed", err)
	}
	status, err := g.Status(context.Background())
	if err != nil || status.StartsReserved != 4 || status.RemainingStarts != 0 || status.ReservedCents != 400 || status.BudgetCents != 500 || status.MarginCents != 100 {
		t.Fatal("wrong conservative reservation", status, err)
	}
	if p.starts.Load() != 4 {
		t.Fatal("provider call count differs from reservation")
	}
}

func TestLocalVerifyGuardOnlyKnownDestinationAndSIDReachCheckFetch(t *testing.T) {
	c, g, p := localGuardSetup(t)
	ctx := context.Background()
	if _, err := g.Start(ctx, phone.StartRequest{DestinationE164: "+525555550003", Channel: phone.SMS}); !errors.Is(err, pe.ErrVerificationInput) {
		t.Fatal(err)
	}
	if _, err := g.Start(ctx, phone.StartRequest{DestinationE164: c.Destinations[0], Channel: "email"}); !errors.Is(err, pe.ErrVerificationInput) {
		t.Fatal(err)
	}
	result, err := g.Start(ctx, localStart(c))
	if err != nil {
		t.Fatal(err)
	}
	g = newLocalGuard(t, c, p)
	correct := phone.Binding{Reference: result.Reference, DestinationE164: c.Destinations[0]}
	for _, binding := range []phone.Binding{
		{Reference: result.Reference, DestinationE164: c.Destinations[1]},
		{Reference: result.Reference, DestinationE164: "+525555550003"},
		{Reference: "VE" + strings.Repeat("9", 32), DestinationE164: c.Destinations[0]},
	} {
		if _, err = g.Check(ctx, phone.CheckRequest{Binding: binding, Code: "123456"}); !errors.Is(err, pe.ErrVerificationInput) {
			t.Fatal("unknown check accepted", err)
		}
		if _, err = g.Fetch(ctx, binding); !errors.Is(err, pe.ErrVerificationInput) {
			t.Fatal("unknown fetch accepted", err)
		}
	}
	if _, err = g.Check(ctx, phone.CheckRequest{Binding: correct, Code: "non-numeric-secret"}); !errors.Is(err, pe.ErrVerificationInput) {
		t.Fatal(err)
	}
	if _, err = g.Check(ctx, phone.CheckRequest{Binding: correct, Code: "123456"}); err != nil {
		t.Fatal(err)
	}
	if _, err = g.Fetch(ctx, correct); err != nil {
		t.Fatal(err)
	}
	if p.starts.Load() != 1 || p.checks.Load() != 1 || p.fetches.Load() != 1 {
		t.Fatal("private provider boundary bypassed")
	}
	raw, err := os.ReadFile(c.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range append(c.Destinations, "123456", "non-numeric-secret") {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatal("ledger contains plaintext destination or code")
		}
	}
	for _, formatted := range []string{fmt.Sprint(g), fmt.Sprintf("%#v", g), fmt.Sprint(c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(formatted, c.LedgerPath) || strings.Contains(formatted, c.Destinations[0]) {
			t.Fatal("private configuration formatted")
		}
	}
}

func TestLocalVerifyGuardStorageFailureBeforeAndAfterSendFailsClosed(t *testing.T) {
	t.Run("before", func(t *testing.T) {
		c, g, p := localGuardSetup(t)
		if err := os.Chmod(c.LedgerPath, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := g.Start(context.Background(), localStart(c)); !errors.Is(err, pe.ErrVerificationConfiguration) {
			t.Fatal(err)
		}
		if p.starts.Load() != 0 {
			t.Fatal("sent without durable private reservation")
		}
	})
	t.Run("after", func(t *testing.T) {
		c, g, p := localGuardSetup(t)
		p.start = func(context.Context, phone.StartRequest) (phone.Result, error) {
			if err := os.Chmod(c.LedgerPath, 0644); err != nil {
				t.Fatal(err)
			}
			return phone.Result{Reference: "VE" + strings.Repeat("a", 32), Channel: phone.SMS, State: phone.Pending}, nil
		}
		result, err := g.Start(context.Background(), localStart(c))
		if !errors.Is(err, pe.ErrVerificationUncertain) || result.Reference != "" {
			t.Fatal("unpersisted binding exposed", err)
		}
		if err = os.Chmod(c.LedgerPath, 0600); err != nil {
			t.Fatal(err)
		}
		status, err := newLocalGuard(t, c, p).Status(context.Background())
		if err != nil || status.StartsReserved != 1 {
			t.Fatal("failed response refunded reservation", err)
		}
		if _, err = g.Fetch(context.Background(), phone.Binding{Reference: "VE" + strings.Repeat("a", 32), DestinationE164: c.Destinations[0]}); !errors.Is(err, pe.ErrVerificationInput) {
			t.Fatal("unpersisted SID accepted", err)
		}
	})
}

func TestLocalVerifyGuardRejectsCorruptionPermissiveFilesAndLinks(t *testing.T) {
	for name, alter := range map[string]func(*testing.T, LocalVerifyGuardConfig){
		"truncated": func(t *testing.T, c LocalVerifyGuardConfig) {
			if err := os.WriteFile(c.LedgerPath, []byte(`{"version":1`), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"missing": func(t *testing.T, c LocalVerifyGuardConfig) {
			if err := os.Remove(c.LedgerPath); err != nil {
				t.Fatal(err)
			}
		},
		"extra budget": func(t *testing.T, c LocalVerifyGuardConfig) {
			b, _ := os.ReadFile(c.LedgerPath)
			b = bytes.Replace(b, []byte(`"maximum_starts":4`), []byte(`"maximum_starts":5`), 1)
			if err := os.WriteFile(c.LedgerPath, b, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"directory permissions": func(t *testing.T, c LocalVerifyGuardConfig) {
			if err := os.Chmod(filepath.Dir(c.LedgerPath), 0755); err != nil {
				t.Fatal(err)
			}
		},
		"lock permissions": func(t *testing.T, c LocalVerifyGuardConfig) {
			if err := os.Chmod(c.LedgerPath+".lock", 0644); err != nil {
				t.Fatal(err)
			}
		},
		"ledger symlink": func(t *testing.T, c LocalVerifyGuardConfig) {
			if err := os.Rename(c.LedgerPath, c.LedgerPath+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(c.LedgerPath+".original", c.LedgerPath); err != nil {
				t.Fatal(err)
			}
		},
		"lock symlink": func(t *testing.T, c LocalVerifyGuardConfig) {
			if err := os.Rename(c.LedgerPath+".lock", c.LedgerPath+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(c.LedgerPath+".original", c.LedgerPath+".lock"); err != nil {
				t.Fatal(err)
			}
		},
		"hardlink": func(t *testing.T, c LocalVerifyGuardConfig) {
			if err := os.Link(c.LedgerPath, c.LedgerPath+".alias"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, g, p := localGuardSetup(t)
			alter(t, c)
			if _, err := g.Start(context.Background(), localStart(c)); !errors.Is(err, pe.ErrVerificationConfiguration) {
				t.Fatal("unsafe storage accepted", err)
			}
			if p.starts.Load() != 0 {
				t.Fatal("unsafe storage reached provider")
			}
			if _, err := newLocalVerifyGuard(c, p); err == nil {
				t.Fatal("restart accepted unsafe storage")
			}
		})
	}
}

func TestLocalVerifyGuardCancellationAndLockWaitDoNotSpend(t *testing.T) {
	c, g, p := localGuardSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Start(ctx, localStart(c)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	lock, err := localPrivateOpen(c.LedgerPath+".lock", unix.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = g.Start(ctx, localStart(c)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lock wait ignored cancellation", err)
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	status, err := g.Status(context.Background())
	if err != nil || status.StartsReserved != 0 || p.starts.Load() != 0 {
		t.Fatal("cancelled pre-reservation call spent slot", err)
	}
}

func TestLocalVerifyGuardConcurrentProcessesShareFourSlots(t *testing.T) {
	if os.Getenv("MANCUERNA_GUARD_TEST_CHILD") == "1" {
		var c LocalVerifyGuardConfig
		if err := json.Unmarshal([]byte(os.Getenv("MANCUERNA_GUARD_TEST_CONFIG")), &c); err != nil {
			t.Fatal(err)
		}
		p := &localGuardProviderDouble{start: func(context.Context, phone.StartRequest) (phone.Result, error) {
			f, err := os.OpenFile(c.LedgerPath+".calls", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			return phone.Result{}, failure(phone.Uncertain)
		}}
		g := newLocalGuard(t, c, p)
		for i := 0; i < 4; i++ {
			_, err := g.Start(context.Background(), localStart(c))
			if !errors.Is(err, pe.ErrVerificationUncertain) && !errors.Is(err, pe.ErrVerificationRateLimited) {
				t.Fatal(err)
			}
		}
		return
	}
	c, g, _ := localGuardSetup(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(binary, "-test.run=^TestLocalVerifyGuardConcurrentProcessesShareFourSlots$")
			cmd.Env = append(os.Environ(), "MANCUERNA_GUARD_TEST_CHILD=1", "MANCUERNA_GUARD_TEST_CONFIG="+string(raw))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("child guard failed: %v %s", err, out)
			}
		}()
	}
	wg.Wait()
	status, err := g.Status(context.Background())
	if err != nil || status.StartsReserved != 4 {
		t.Fatal("lost cross-process reservation", err)
	}
	called, err := os.ReadFile(c.LedgerPath + ".calls")
	if err != nil || string(called) != "xxxx" {
		t.Fatal("more than four provider calls across processes", err)
	}
}

func TestLocalVerifyGuardPublicConstructorPinsActualProvider(t *testing.T) {
	c, _, _ := localGuardSetup(t)
	provider, err := NewTwilio(TwilioConfig{AccountSID: c.AccountSID, ServiceSID: c.ServiceSID, Username: c.AccountSID, Password: "synthetic-private-password", ServiceValidity: 10 * time.Minute, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewLocalVerifyGuard(c, provider); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Twilio){
		"account":  func(p *Twilio) { p.config.AccountSID = "AC" + strings.Repeat("4", 32) },
		"service":  func(p *Twilio) { p.config.ServiceSID = "VA" + strings.Repeat("4", 32) },
		"endpoint": func(p *Twilio) { p.endpoint = "http://127.0.0.1" },
	} {
		t.Run(name, func(t *testing.T) {
			altered := *provider
			change(&altered)
			if _, err := NewLocalVerifyGuard(c, &altered); !errors.Is(err, pe.ErrVerificationConfiguration) {
				t.Fatal("different actual provider accepted", err)
			}
		})
	}
}

func TestLocalVerifyGuardDoesNotBlockOnNonRegularFiles(t *testing.T) {
	c, g, p := localGuardSetup(t)
	if err := os.Remove(c.LedgerPath); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(c.LedgerPath, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Start(context.Background(), localStart(c)); !errors.Is(err, pe.ErrVerificationConfiguration) {
		t.Fatal("FIFO accepted", err)
	}
	if p.starts.Load() != 0 {
		t.Fatal("non-regular storage reached provider")
	}
}

func TestLocalVerifyGuardPersistsKnownSIDAfterCallerCancellation(t *testing.T) {
	c, g, p := localGuardSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.start = func(context.Context, phone.StartRequest) (phone.Result, error) {
		cancel()
		return phone.Result{Reference: "VE" + strings.Repeat("e", 32), Channel: phone.SMS, State: phone.Pending}, nil
	}
	result, err := g.Start(ctx, localStart(c))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = newLocalGuard(t, c, p).Fetch(context.Background(), phone.Binding{Reference: result.Reference, DestinationE164: c.Destinations[0]}); err != nil {
		t.Fatal("cancelled caller lost durable SID", err)
	}
}

func TestLocalVerifyGuardProcessExitKeepsReservation(t *testing.T) {
	if os.Getenv("MANCUERNA_GUARD_TEST_CRASH") == "1" {
		var c LocalVerifyGuardConfig
		if err := json.Unmarshal([]byte(os.Getenv("MANCUERNA_GUARD_TEST_CONFIG")), &c); err != nil {
			t.Fatal(err)
		}
		p := &localGuardProviderDouble{start: func(context.Context, phone.StartRequest) (phone.Result, error) {
			os.Exit(17)
			return phone.Result{}, nil
		}}
		_, _ = newLocalGuard(t, c, p).Start(context.Background(), localStart(c))
		t.Fatal("child did not exit during provider call")
	}
	c, _, p := localGuardSetup(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestLocalVerifyGuardProcessExitKeepsReservation$")
	cmd.Env = append(os.Environ(), "MANCUERNA_GUARD_TEST_CRASH=1", "MANCUERNA_GUARD_TEST_CONFIG="+string(raw))
	if err = cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 17 {
		t.Fatal("fixture did not exit at provider boundary", err)
	}
	status, err := newLocalGuard(t, c, p).Status(context.Background())
	if err != nil || status.StartsReserved != 1 || status.RemainingStarts != 3 {
		t.Fatal("process exit refunded reservation", err)
	}
}
