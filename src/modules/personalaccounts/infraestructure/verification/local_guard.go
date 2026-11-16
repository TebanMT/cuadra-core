//go:build server && (darwin || linux)

package verification

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	phone "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// This guard is only for the explicitly authorized local Verify exercise. The
// reservation is conservative, not a quote or a representation of a real bill.
const LocalVerifyMaximumStarts = 4
const LocalVerifyReserveCents = 100
const LocalVerifyBudgetCents = 500

type LocalVerifyGuardConfig struct {
	LedgerPath   string
	AccountSID   string
	ServiceSID   string
	Destinations []string
}

func (LocalVerifyGuardConfig) String() string {
	return "local Verify guard configuration (private values redacted)"
}
func (c LocalVerifyGuardConfig) GoString() string { return c.String() }

type LocalVerifyGuardStatus struct {
	StartsReserved  int
	RemainingStarts int
	ReservedCents   int
	BudgetCents     int
	MarginCents     int
}

type LocalVerifyGuard struct {
	config   LocalVerifyGuardConfig
	hashes   []string
	ledgerID string
	provider phone.VerificationProvider
}

func (*LocalVerifyGuard) String() string     { return "local Verify guard (private values redacted)" }
func (g *LocalVerifyGuard) GoString() string { return g.String() }

var _ phone.VerificationProvider = (*LocalVerifyGuard)(nil)

type localVerifyLedger struct {
	Version       int                `json:"version"`
	ID            string             `json:"id"`
	AccountSID    string             `json:"account_sid"`
	ServiceSID    string             `json:"service_sid"`
	Destinations  []string           `json:"destination_hashes"`
	MaximumStarts int                `json:"maximum_starts"`
	ReserveCents  int                `json:"reserve_cents"`
	BudgetCents   int                `json:"budget_cents"`
	Starts        []localVerifyStart `json:"starts"`
}

type localVerifyStart struct {
	Number      int           `json:"number"`
	Destination string        `json:"destination_hash"`
	Channel     phone.Channel `json:"channel"`
	ReservedAt  time.Time     `json:"reserved_at"`
	Reference   string        `json:"reference,omitempty"`
}

func validateLocalVerifyConfig(c LocalVerifyGuardConfig) ([]string, error) {
	if !filepath.IsAbs(c.LedgerPath) || filepath.Clean(c.LedgerPath) != c.LedgerPath ||
		!accountSID.MatchString(c.AccountSID) || !serviceSID.MatchString(c.ServiceSID) || len(c.Destinations) != 2 {
		return nil, failure(phone.InvalidConfiguration)
	}
	hashes := make([]string, 0, 2)
	for _, destination := range c.Destinations {
		if !e164.MatchString(destination) {
			return nil, failure(phone.InvalidConfiguration)
		}
		hashes = append(hashes, localDestinationHash(c, destination))
	}
	sort.Strings(hashes)
	if hashes[0] == hashes[1] {
		return nil, failure(phone.InvalidConfiguration)
	}
	if err := localPrivateDirectory(filepath.Dir(c.LedgerPath)); err != nil {
		return nil, err
	}
	return hashes, nil
}

func localDestinationHash(c LocalVerifyGuardConfig, destination string) string {
	hash := sha256.Sum256([]byte("mancuerna:local-verify:destination:v1\x00" + c.AccountSID + "\x00" + c.ServiceSID + "\x00" + destination))
	return hex.EncodeToString(hash[:])
}

// InitializeLocalVerifyLedger is an explicit setup action, never a startup
// fallback. A pre-existing ledger OR lock file prevents reinitialization. A
// failed initialization can leave a locked-out directory requiring review.
func InitializeLocalVerifyLedger(c LocalVerifyGuardConfig) error {
	hashes, err := validateLocalVerifyConfig(c)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(c.LedgerPath); !errors.Is(err, os.ErrNotExist) {
		return failure(phone.InvalidConfiguration)
	}
	lock, err := localPrivateOpen(c.LedgerPath+".lock", unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return failure(phone.InvalidConfiguration)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	ledger := localVerifyLedger{Version: 1, ID: uuid.NewString(), AccountSID: c.AccountSID, ServiceSID: c.ServiceSID, Destinations: hashes, MaximumStarts: LocalVerifyMaximumStarts, ReserveCents: LocalVerifyReserveCents, BudgetCents: LocalVerifyBudgetCents, Starts: []localVerifyStart{}}
	// O_EXCL also protects against an unexpected file created after Lstat.
	f, err := localPrivateOpen(c.LedgerPath, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := json.Marshal(ledger)
	if err != nil {
		return failure(phone.InvalidConfiguration)
	}
	if _, err = f.Write(raw); err != nil {
		return failure(phone.InvalidConfiguration)
	}
	if err = f.Sync(); err != nil {
		return failure(phone.InvalidConfiguration)
	}
	if err = lock.Sync(); err != nil {
		return failure(phone.InvalidConfiguration)
	}
	return localSyncDirectory(filepath.Dir(c.LedgerPath))
}

// NewLocalVerifyGuard requires the same private durable ledger on every
// restart, outside a disposable test database. It performs no provider I/O.
func NewLocalVerifyGuard(c LocalVerifyGuardConfig, provider *Twilio) (*LocalVerifyGuard, error) {
	if provider == nil || provider.config.AccountSID != c.AccountSID || provider.config.ServiceSID != c.ServiceSID || provider.endpoint != twilioEndpoint {
		return nil, failure(phone.InvalidConfiguration)
	}
	return newLocalVerifyGuard(c, provider)
}

// Synthetic provider injection is confined to package tests. Runtime callers
// must use the fixed Twilio endpoint and the exact pinned account/service.
func newLocalVerifyGuard(c LocalVerifyGuardConfig, provider phone.VerificationProvider) (*LocalVerifyGuard, error) {
	hashes, err := validateLocalVerifyConfig(c)
	if err != nil || provider == nil {
		return nil, failure(phone.InvalidConfiguration)
	}
	c.Destinations = append([]string(nil), c.Destinations...)
	g := &LocalVerifyGuard{config: c, hashes: hashes, provider: provider}
	err = g.withLedger(context.Background(), func(ledger *localVerifyLedger) (bool, error) { g.ledgerID = ledger.ID; return false, nil })
	if err != nil {
		return nil, err
	}
	return g, nil
}

func (g *LocalVerifyGuard) Status(ctx context.Context) (LocalVerifyGuardStatus, error) {
	var status LocalVerifyGuardStatus
	err := g.withLedger(ctx, func(ledger *localVerifyLedger) (bool, error) {
		status = LocalVerifyGuardStatus{StartsReserved: len(ledger.Starts), RemainingStarts: LocalVerifyMaximumStarts - len(ledger.Starts), ReservedCents: len(ledger.Starts) * LocalVerifyReserveCents, BudgetCents: LocalVerifyBudgetCents, MarginCents: LocalVerifyBudgetCents - LocalVerifyMaximumStarts*LocalVerifyReserveCents}
		return false, nil
	})
	return status, err
}

func (g *LocalVerifyGuard) Start(ctx context.Context, in phone.StartRequest) (phone.Result, error) {
	if g == nil || !g.allowed(in.DestinationE164) || (in.Channel != phone.SMS && in.Channel != phone.WhatsApp) {
		return phone.Result{}, failure(phone.InvalidInput)
	}
	destination := localDestinationHash(g.config, in.DestinationE164)
	number := 0
	err := g.withLedger(ctx, func(ledger *localVerifyLedger) (bool, error) {
		if len(ledger.Starts) >= LocalVerifyMaximumStarts {
			return false, failure(phone.RateLimited)
		}
		number = len(ledger.Starts) + 1
		ledger.Starts = append(ledger.Starts, localVerifyStart{Number: number, Destination: destination, Channel: in.Channel, ReservedAt: time.Now().UTC()})
		return true, nil
	})
	if err != nil {
		return phone.Result{}, err
	}
	// The reservation is already durable. Cancellation, timeout, rejected send
	// or process exit can never refund it. No filesystem lock spans this call.
	result, err := g.provider.Start(ctx, in)
	if err != nil {
		return phone.Result{}, err
	}
	if !verificationSID.MatchString(result.Reference) || result.Channel != in.Channel {
		return phone.Result{}, failure(phone.Uncertain)
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err = g.withLedger(finish, func(ledger *localVerifyLedger) (bool, error) {
		if number < 1 || number > len(ledger.Starts) {
			return false, failure(phone.InvalidConfiguration)
		}
		entry := &ledger.Starts[number-1]
		if entry.Destination != destination || entry.Channel != in.Channel || entry.Reference != "" {
			return false, failure(phone.InvalidConfiguration)
		}
		for _, other := range ledger.Starts {
			if other.Reference == result.Reference && other.Destination != destination {
				return false, failure(phone.InvalidConfiguration)
			}
		}
		entry.Reference = result.Reference
		return true, nil
	})
	if err != nil {
		return phone.Result{}, failure(phone.Uncertain)
	}
	return result, nil
}

func (g *LocalVerifyGuard) Check(ctx context.Context, in phone.CheckRequest) (phone.Result, error) {
	if !codePattern.MatchString(in.Code) {
		return phone.Result{}, failure(phone.InvalidInput)
	}
	if err := g.requireKnown(ctx, in.Binding); err != nil {
		return phone.Result{}, err
	}
	return g.provider.Check(ctx, in)
}
func (g *LocalVerifyGuard) Fetch(ctx context.Context, in phone.Binding) (phone.Result, error) {
	if err := g.requireKnown(ctx, in); err != nil {
		return phone.Result{}, err
	}
	return g.provider.Fetch(ctx, in)
}
func (g *LocalVerifyGuard) allowed(destination string) bool {
	if g == nil || !e164.MatchString(destination) {
		return false
	}
	hash := localDestinationHash(g.config, destination)
	return len(g.hashes) == 2 && (hash == g.hashes[0] || hash == g.hashes[1])
}
func (g *LocalVerifyGuard) requireKnown(ctx context.Context, binding phone.Binding) error {
	if g == nil || !g.allowed(binding.DestinationE164) || !validBinding(binding) {
		return failure(phone.InvalidInput)
	}
	hash := localDestinationHash(g.config, binding.DestinationE164)
	return g.withLedger(ctx, func(ledger *localVerifyLedger) (bool, error) {
		for _, entry := range ledger.Starts {
			if entry.Reference == binding.Reference && entry.Destination == hash {
				return false, nil
			}
		}
		return false, failure(phone.InvalidInput)
	})
}

func (g *LocalVerifyGuard) withLedger(ctx context.Context, fn func(*localVerifyLedger) (bool, error)) error {
	if g == nil || ctx == nil || fn == nil {
		return failure(phone.InvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := localPrivateDirectory(filepath.Dir(g.config.LedgerPath)); err != nil {
		return err
	}
	lock, err := localPrivateOpen(g.config.LedgerPath+".lock", unix.O_RDWR)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return failure(phone.InvalidConfiguration)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := localPrivateOpen(g.config.LedgerPath, unix.O_RDONLY)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	_ = f.Close()
	if err != nil || len(raw) > 64*1024 {
		return failure(phone.InvalidConfiguration)
	}
	var ledger localVerifyLedger
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ledger) != nil {
		return failure(phone.InvalidConfiguration)
	}
	canonical, err := json.Marshal(ledger)
	if err != nil || !bytes.Equal(raw, canonical) || !g.validLedger(ledger) {
		return failure(phone.InvalidConfiguration)
	}
	changed, err := fn(&ledger)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if !g.validLedger(ledger) {
		return failure(phone.InvalidConfiguration)
	}
	return localReplaceLedger(g.config.LedgerPath, ledger)
}

func (g *LocalVerifyGuard) validLedger(l localVerifyLedger) bool {
	id, err := uuid.Parse(l.ID)
	if err != nil || id.Version() != 4 || id.String() != l.ID || (g.ledgerID != "" && l.ID != g.ledgerID) ||
		l.Version != 1 || l.AccountSID != g.config.AccountSID || l.ServiceSID != g.config.ServiceSID ||
		l.MaximumStarts != LocalVerifyMaximumStarts || l.ReserveCents != LocalVerifyReserveCents || l.BudgetCents != LocalVerifyBudgetCents ||
		len(l.Destinations) != 2 || l.Destinations[0] != g.hashes[0] || l.Destinations[1] != g.hashes[1] || l.Starts == nil || len(l.Starts) > LocalVerifyMaximumStarts {
		return false
	}
	refs := map[string]string{}
	for i, entry := range l.Starts {
		if entry.Number != i+1 || (entry.Destination != g.hashes[0] && entry.Destination != g.hashes[1]) ||
			(entry.Channel != phone.SMS && entry.Channel != phone.WhatsApp) || entry.ReservedAt.IsZero() || entry.ReservedAt.Location() != time.UTC {
			return false
		}
		if entry.Reference != "" {
			if !verificationSID.MatchString(entry.Reference) {
				return false
			}
			if previous, ok := refs[entry.Reference]; ok && previous != entry.Destination {
				return false
			}
			refs[entry.Reference] = entry.Destination
		}
	}
	return true
}

func localPrivateDirectory(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return failure(phone.InvalidConfiguration)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return failure(phone.InvalidConfiguration)
	}
	var stat unix.Stat_t
	if unix.Lstat(path, &stat) != nil || stat.Uid != uint32(os.Geteuid()) {
		return failure(phone.InvalidConfiguration)
	}
	return nil
}

func localPrivateOpen(path string, flags int) (*os.File, error) {
	// O_NONBLOCK prevents a substituted FIFO/device from blocking before the
	// regular-file check below; it has no effect on normal ledger files.
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, failure(phone.InvalidConfiguration)
	}
	f := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0777 != 0600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		_ = f.Close()
		return nil, failure(phone.InvalidConfiguration)
	}
	return f, nil
}

func localReplaceLedger(path string, ledger localVerifyLedger) error {
	raw, err := json.Marshal(ledger)
	if err != nil {
		return failure(phone.InvalidConfiguration)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".verify-ledger-")
	if err != nil {
		return failure(phone.InvalidConfiguration)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(raw); err != nil {
		return failure(phone.InvalidConfiguration)
	}
	if err = f.Sync(); err != nil {
		return failure(phone.InvalidConfiguration)
	}
	if err = f.Close(); err != nil {
		return failure(phone.InvalidConfiguration)
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return failure(phone.InvalidConfiguration)
	}
	return localSyncDirectory(filepath.Dir(path))
}

func localSyncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return failure(phone.InvalidConfiguration)
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return failure(phone.InvalidConfiguration)
	}
	return nil
}
