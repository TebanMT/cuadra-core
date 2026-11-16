//go:build server && (darwin || linux)

// Local setup helper. No command invokes a provider operation.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	verify "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/verification"
)

func privateFile(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("private input unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	s, err := f.Stat()
	link, lerr := os.Lstat(path)
	if err != nil || lerr != nil || !s.Mode().IsRegular() || !os.SameFile(s, link) || s.Mode().Perm() != 0600 || s.Size() > 65536 {
		return nil, errors.New("invalid private input")
	}
	owned, ok := s.Sys().(*syscall.Stat_t)
	if !ok || owned.Uid != uint32(os.Getuid()) || owned.Nlink != 1 {
		return nil, errors.New("invalid private input")
	}
	return io.ReadAll(io.LimitReader(f, 65537))
}

func run(command, path string) error {
	if command == "keys" {
		key := func() (string, error) {
			b := make([]byte, 32)
			_, err := rand.Read(b)
			return base64.RawURLEncoding.EncodeToString(b), err
		}
		keys := make([]string, 6)
		for i := range keys {
			var err error
			keys[i], err = key()
			if err != nil {
				return err
			}
		}
		seed, _ := base64.RawURLEncoding.DecodeString(keys[0])
		public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
		ring := func(k string) any {
			return map[string]any{"active": "local-v1", "keys": map[string]string{"local-v1": k}}
		}
		doc := map[string]any{"jwt_key_id": "local-v1", "jwt_seed": keys[0], "jwt_verification_keys": map[string]string{"local-v1": base64.RawURLEncoding.EncodeToString(public)}, "envelope_keys": ring(keys[1]), "recovery_keys": ring(keys[2]), "cursor_keys": ring(keys[3]), "phone_lock_key": keys[4], "budget_hash_key": keys[5]}
		raw, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err = f.Write(raw); err != nil {
			return err
		}
		return f.Sync()
	}
	raw, err := privateFile(path)
	if err != nil {
		return err
	}
	var c struct {
		Verify struct {
			AccountSID      string `json:"account_sid"`
			ServiceSID      string `json:"service_sid"`
			Username        string `json:"username"`
			Password        string `json:"password"`
			ValiditySeconds int    `json:"validity_seconds"`
			TimeoutSeconds  int    `json:"timeout_seconds"`
		} `json:"verify"`
		Guard struct {
			LedgerPath   string   `json:"ledger_path"`
			Destinations []string `json:"destinations"`
		} `json:"local_verify_guard"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return errors.New("invalid private configuration")
	}
	g := verify.LocalVerifyGuardConfig{LedgerPath: c.Guard.LedgerPath, AccountSID: c.Verify.AccountSID, ServiceSID: c.Verify.ServiceSID, Destinations: c.Guard.Destinations}
	if command == "ledger-init" {
		return verify.InitializeLocalVerifyLedger(g)
	}
	if command != "ledger-status" {
		return errors.New("unknown command")
	}
	p, err := verify.NewTwilio(verify.TwilioConfig{AccountSID: c.Verify.AccountSID, ServiceSID: c.Verify.ServiceSID, Username: c.Verify.Username, Password: c.Verify.Password, ServiceValidity: time.Duration(c.Verify.ValiditySeconds) * time.Second, RequestTimeout: time.Duration(c.Verify.TimeoutSeconds) * time.Second})
	if err != nil {
		return err
	}
	guard, err := verify.NewLocalVerifyGuard(g, p)
	if err != nil {
		return err
	}
	status, err := guard.Status(context.Background())
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}

func main() {
	if len(os.Args) != 3 || run(os.Args[1], os.Args[2]) != nil {
		// Never format wrapped errors: a filesystem path or decoder error can
		// include private material. The caller reports only the failed stage.
		fmt.Fprintln(os.Stderr, "local setup helper failed")
		os.Exit(1)
	}
}
